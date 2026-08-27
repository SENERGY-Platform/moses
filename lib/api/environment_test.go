/*
 * Copyright 2026 InfAI (CC SES)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	moses_runtime "github.com/SENERGY-Platform/moses/lib/runtime"
	"github.com/SENERGY-Platform/moses/lib/test/helper"
	"github.com/gin-gonic/gin"
)

// fakeEnvironments is an in-memory repo.Environments. The handlers are tested
// against the real interface rather than a mock of each call, so the assertions
// are about stored values instead of call counts.
type fakeEnvironments struct {
	stored  map[string]domain.Environment
	failing error
}

func newFakeEnvironments() *fakeEnvironments {
	return &fakeEnvironments{stored: map[string]domain.Environment{}}
}

func (this *fakeEnvironments) Put(ctx context.Context, env domain.Environment) error {
	if this.failing != nil {
		return this.failing
	}
	this.stored[env.Id] = env
	return nil
}

func (this *fakeEnvironments) Get(ctx context.Context, id string) (domain.Environment, error) {
	if this.failing != nil {
		return domain.Environment{}, this.failing
	}
	env, ok := this.stored[id]
	if !ok {
		return domain.Environment{}, fmt.Errorf("looking for %v: %w", id, repo.ErrNotFound)
	}
	return env, nil
}

func (this *fakeEnvironments) ListByOwner(ctx context.Context, owner string) ([]domain.Environment, error) {
	if this.failing != nil {
		return nil, this.failing
	}
	result := []domain.Environment{}
	for _, env := range this.stored {
		if env.Owner == owner {
			result = append(result, env)
		}
	}
	sort.Slice(result, func(a, b int) bool { return result[a].Name < result[b].Name })
	return result, nil
}

func (this *fakeEnvironments) All(ctx context.Context) ([]domain.Environment, error) {
	if this.failing != nil {
		return nil, this.failing
	}
	result := []domain.Environment{}
	for _, env := range this.stored {
		result = append(result, env)
	}
	return result, nil
}

func (this *fakeEnvironments) Delete(ctx context.Context, id string) error {
	if this.failing != nil {
		return this.failing
	}
	delete(this.stored, id)
	return nil
}

// tokenFor builds an unsigned token for a user id. requireUser only parses the
// token, and the shared test token in lib/test/helper carries the admin role,
// which would bypass every ownership check.
func tokenFor(userId string) string {
	payload, err := json.Marshal(map[string]interface{}{
		"sub":          userId,
		"realm_access": map[string]interface{}{"roles": []string{"user"}},
	})
	if err != nil {
		panic(err)
	}
	encode := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return "Bearer " + encode([]byte(`{"alg":"none"}`)) + "." + encode(payload) + ".signature"
}

// adminTokenFor is tokenFor plus the admin realm role, which is what the
// platform's gateway passes through for an administrator.
func adminTokenFor(userId string) string {
	payload, err := json.Marshal(map[string]interface{}{
		"sub":          userId,
		"realm_access": map[string]interface{}{"roles": []string{"user", "admin"}},
	})
	if err != nil {
		panic(err)
	}
	encode := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return "Bearer " + encode([]byte(`{"alg":"none"}`)) + "." + encode(payload) + ".signature"
}

func doAsAdmin(t *testing.T, router *gin.Engine, method string, path string, userId string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(nil))
	request.Header.Set("Authorization", adminTokenFor(userId))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// testRouterWithCatalog wires a device catalog in, which is what turns an
// asset with a device type but no device into a provisioned one on store.
func testRouterWithCatalog(store repo.Environments, catalog DeviceCatalog) *gin.Engine {
	//nil mirror: the handlers have to work without a device-repository behind
	//them, which is what every test that is not about the graph mirror wants
	return testRouterWith(store, catalog, nil, nil)
}

func testRouter(store repo.Environments) *gin.Engine {
	//nil notifier: the handlers have to work without a runtime behind them, which
	//is also what a store only deployment looks like
	return testRouterWithNotifier(store, nil)
}

func testRouterWithNotifier(store repo.Environments, notifier RuntimeNotifier) *gin.Engine {
	return testRouterWith(store, nil, nil, notifier)
}

// testRouterWith is the one place the environment endpoints are wired for a
// test, so a new collaborator does not have to be threaded through every helper.
func testRouterWith(store repo.Environments, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	EnvironmentEndpoints(config.Config{}, store, catalog, mirror, notifier, router)
	BackfillEndpoints(config.Config{}, store, catalog, mirror, notifier, router)
	return router
}

// recordingNotifier is the RuntimeNotifier a test watches. It records ids rather
// than counting calls, because which environment was affected is the point: the
// runtime must be told about exactly the one that changed.
type recordingNotifier struct {
	reloaded []string
	removed  []string
	changes  []repo.StateChange
	setErr   error

	// backfills records the windows StartBackfill was asked for, and
	// startErr/statusErr are what the two backfill calls answer with, so a test
	// can pin every status code the handlers map.
	backfills  []moses_runtime.BackfillStatus
	startErr   error
	status     moses_runtime.BackfillStatus
	statusErr  error
	statusCall int
}

func (this *recordingNotifier) Reload(id string) { this.reloaded = append(this.reloaded, id) }
func (this *recordingNotifier) Remove(id string) { this.removed = append(this.removed, id) }

func (this *recordingNotifier) SetState(id string, change repo.StateChange) error {
	if this.setErr != nil {
		return this.setErr
	}
	this.changes = append(this.changes, change)
	return nil
}

func (this *recordingNotifier) StartBackfill(id string, from time.Time, to time.Time) (moses_runtime.BackfillStatus, error) {
	if this.startErr != nil {
		return moses_runtime.BackfillStatus{}, this.startErr
	}
	status := moses_runtime.BackfillStatus{
		EnvironmentId: id, State: moses_runtime.BackfillRunning, From: from, To: to,
	}
	this.backfills = append(this.backfills, status)
	return status, nil
}

func (this *recordingNotifier) BackfillStatusOf(id string) (moses_runtime.BackfillStatus, error) {
	this.statusCall++
	if this.statusErr != nil {
		return moses_runtime.BackfillStatus{}, this.statusErr
	}
	return this.status, nil
}

func TestTheRuntimeIsToldAboutExactlyTheChangedEnvironment(t *testing.T) {
	store := newFakeEnvironments()
	notifier := &recordingNotifier{}
	router := testRouterWithNotifier(store, notifier)

	if resp := do(t, router, "PUT", "/environments/env-1", "user-a", minimalEnvironment()); resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(notifier.reloaded) != 1 || notifier.reloaded[0] != "env-1" {
		t.Errorf("expected a reload of env-1 after a put, got %#v", notifier.reloaded)
	}

	created := domain.Environment{}
	resp := do(t, router, "POST", "/environments", "user-a", minimalEnvironment())
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.Code, resp.Body.String())
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(notifier.reloaded) != 2 || notifier.reloaded[1] != created.Id {
		t.Errorf("expected a reload of the created id %v, got %#v", created.Id, notifier.reloaded)
	}

	if resp := do(t, router, "DELETE", "/environments/env-1", "user-a", nil); resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(notifier.removed) != 1 || notifier.removed[0] != "env-1" {
		t.Errorf("expected a removal of env-1 after a delete, got %#v", notifier.removed)
	}
	if len(notifier.reloaded) != 2 {
		t.Errorf("a delete must not reload anything, got %#v", notifier.reloaded)
	}
}

// TestTheRuntimeIsNotToldAboutAWriteThatDidNotHappen: a reload on a rejected or
// failed write would restart an environment for nothing, and on a failed delete
// it would stop one that still exists.
func TestTheRuntimeIsNotToldAboutAWriteThatDidNotHappen(t *testing.T) {
	store := newFakeEnvironments()
	notifier := &recordingNotifier{}
	router := testRouterWithNotifier(store, notifier)

	invalid := minimalEnvironment()
	invalid.Name = ""
	if resp := do(t, router, "PUT", "/environments/env-1", "user-a", invalid); resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
	}

	store.failing = errors.New("database is down")
	if resp := do(t, router, "PUT", "/environments/env-2", "user-a", minimalEnvironment()); resp.Code < 400 {
		t.Fatalf("expected a failure, got %d", resp.Code)
	}
	store.failing = nil

	if len(notifier.reloaded) != 0 || len(notifier.removed) != 0 {
		t.Errorf("expected no notification at all, got reloads %#v and removals %#v", notifier.reloaded, notifier.removed)
	}
}

func do(t *testing.T, router *gin.Engine, method string, path string, userId string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	if userId != "" {
		request.Header.Set("Authorization", tokenFor(userId))
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func minimalEnvironment() domain.Environment {
	return domain.Environment{
		Name: "Metallbau Musterstadt",
		Type: domain.IndustrialSite,
		Zones: []domain.Zone{{
			Name: "Halle 1",
			Type: domain.ZoneHall,
			Assets: []domain.Asset{{
				Name:           "Hauptzähler Strom",
				Kind:           domain.AssetMeter,
				ExternalTypeId: "urn:infai:ses:device-type:abc",
				Channels: []domain.Channel{{
					Name:            "Wirkenergie",
					Direction:       domain.Sensor,
					Unit:            "kWh",
					IntervalSeconds: 30,
					Source:          domain.Source{Kind: domain.SourceScript, Script: &domain.ScriptSource{Code: "moses.service.send(1);"}},
				}},
			}},
		}},
	}
}

func TestPutStoresAnEnvironmentAndAssignsMissingIds(t *testing.T) {
	store := newFakeEnvironments()
	resp := do(t, testRouter(store), "PUT", "/environments/env-1", "user-a", minimalEnvironment())
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	stored, ok := store.stored["env-1"]
	if !ok {
		t.Fatal("expected the environment to be stored under the id from the path")
	}
	if stored.Owner != "user-a" {
		t.Fatalf("expected the owner to come from the token, got %q", stored.Owner)
	}
	if stored.Zones[0].Id == "" || stored.Zones[0].Assets[0].Id == "" || stored.Zones[0].Assets[0].Channels[0].Id == "" {
		t.Fatal("expected ids to be assigned throughout the tree")
	}
}

func TestPutIsIdempotent(t *testing.T) {
	store := newFakeEnvironments()
	router := testRouter(store)

	first := do(t, router, "PUT", "/environments/env-1", "user-a", minimalEnvironment())
	if first.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", first.Code, first.Body.String())
	}
	returned := domain.Environment{}
	if err := json.Unmarshal(first.Body.Bytes(), &returned); err != nil {
		t.Fatal(err)
	}

	// putting back exactly what was returned must not change anything
	second := do(t, router, "PUT", "/environments/env-1", "user-a", returned)
	if second.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", second.Code, second.Body.String())
	}
	again := domain.Environment{}
	if err := json.Unmarshal(second.Body.Bytes(), &again); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, returned, again) {
		t.Fatalf("expected the second put to be a no-op\nfirst:  %s\nsecond: %s", first.Body.String(), second.Body.String())
	}
}

func TestPutUsesTheIdFromThePathSoADocumentCanBeCopied(t *testing.T) {
	store := newFakeEnvironments()
	env := minimalEnvironment()
	env.Id = "original"
	resp := do(t, testRouter(store), "PUT", "/environments/copy", "user-a", env)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if _, ok := store.stored["copy"]; !ok {
		t.Fatal("expected the environment to be stored under the path id")
	}
	if _, ok := store.stored["original"]; ok {
		t.Fatal("expected the id in the body to be ignored")
	}
}

func TestPutDoesNotTransferOwnershipOnUpdate(t *testing.T) {
	store := newFakeEnvironments()
	existing := minimalEnvironment()
	existing.Id = "env-1"
	existing.Owner = "user-a"
	store.stored["env-1"] = existing

	// the owner updates it and tries to hand it to somebody else
	env := minimalEnvironment()
	env.Owner = "user-b"
	resp := do(t, testRouter(store), "PUT", "/environments/env-1", "user-a", env)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if store.stored["env-1"].Owner != "user-a" {
		t.Fatalf("expected the owner to stay user-a, got %q", store.stored["env-1"].Owner)
	}
}

func TestPutOnAForeignEnvironmentReportsNotFound(t *testing.T) {
	store := newFakeEnvironments()
	existing := minimalEnvironment()
	existing.Id = "env-1"
	existing.Owner = "user-a"
	store.stored["env-1"] = existing

	resp := do(t, testRouter(store), "PUT", "/environments/env-1", "user-b", minimalEnvironment())
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404 so that existence is not disclosed, got %d", resp.Code)
	}
	if store.stored["env-1"].Owner != "user-a" {
		t.Fatal("expected the foreign environment to be untouched")
	}
}

func TestPutReportsEveryInvalidFieldWithItsPath(t *testing.T) {
	store := newFakeEnvironments()
	env := minimalEnvironment()
	env.Zones[0].Assets[0].ExternalTypeId = ""
	env.Zones[0].Assets[0].Channels[0].Direction = "sideways"

	resp := do(t, testRouter(store), "PUT", "/environments/env-1", "user-a", env)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
	}
	reported := domain.ValidationError{}
	if err := json.Unmarshal(resp.Body.Bytes(), &reported); err != nil {
		t.Fatalf("expected a machine readable problem list, got %s", resp.Body.String())
	}
	paths := map[string]bool{}
	for _, p := range reported.Problems {
		paths[p.Path] = true
	}
	for _, want := range []string{"zones[0].assets[0].external_type_id", "zones[0].assets[0].channels[0].direction"} {
		if !paths[want] {
			t.Fatalf("expected a problem at %q, got %v", want, reported.Problems)
		}
	}
	if len(store.stored) != 0 {
		t.Fatal("expected nothing to be stored for an invalid document")
	}
}

func TestGetReturnsWhatPutAccepts(t *testing.T) {
	store := newFakeEnvironments()
	router := testRouter(store)
	put := do(t, router, "PUT", "/environments/env-1", "user-a", minimalEnvironment())
	if put.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", put.Code, put.Body.String())
	}

	get := do(t, router, "GET", "/environments/env-1", "user-a", nil)
	if get.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", get.Code)
	}
	exported := domain.Environment{}
	if err := json.Unmarshal(get.Body.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	// the export must be accepted back unchanged, that is what makes it an export
	back := do(t, router, "PUT", "/environments/env-1", "user-a", exported)
	if back.Code != http.StatusOK {
		t.Fatalf("expected the export to be accepted back, got %d: %s", back.Code, back.Body.String())
	}
}

func TestGetNeverDisclosesTheOwner(t *testing.T) {
	store := newFakeEnvironments()
	env := minimalEnvironment()
	env.Id = "env-1"
	env.Owner = "user-a"
	store.stored["env-1"] = env

	resp := do(t, testRouter(store), "GET", "/environments/env-1", "user-a", nil)
	body := map[string]interface{}{}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, present := body["owner"]; present {
		t.Fatal("the owner must not be serialised, an import could otherwise claim it")
	}
}

func TestGetOnAForeignEnvironmentReportsNotFound(t *testing.T) {
	store := newFakeEnvironments()
	env := minimalEnvironment()
	env.Id = "env-1"
	env.Owner = "user-a"
	store.stored["env-1"] = env

	resp := do(t, testRouter(store), "GET", "/environments/env-1", "user-b", nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.Code)
	}
}

func TestListReturnsOnlyTheCallersEnvironmentsAsAnArray(t *testing.T) {
	store := newFakeEnvironments()
	mine := minimalEnvironment()
	mine.Id, mine.Owner, mine.Name = "mine", "user-a", "Alpha"
	foreign := minimalEnvironment()
	foreign.Id, foreign.Owner, foreign.Name = "foreign", "user-b", "Beta"
	store.stored["mine"] = mine
	store.stored["foreign"] = foreign

	resp := do(t, testRouter(store), "GET", "/environments", "user-a", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Code)
	}
	list := []domain.Environment{}
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Id != "mine" {
		t.Fatalf("expected only the caller's environment, got %v", list)
	}
}

func TestListReturnsAnEmptyArrayRatherThanNull(t *testing.T) {
	resp := do(t, testRouter(newFakeEnvironments()), "GET", "/environments", "user-a", nil)
	if got := bytes.TrimSpace(resp.Body.Bytes()); string(got) != "[]" {
		t.Fatalf("expected [], got %s", got)
	}
}

func TestDeleteRemovesTheEnvironment(t *testing.T) {
	store := newFakeEnvironments()
	env := minimalEnvironment()
	env.Id, env.Owner = "env-1", "user-a"
	store.stored["env-1"] = env

	resp := do(t, testRouter(store), "DELETE", "/environments/env-1", "user-a", nil)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.Code)
	}
	if len(store.stored) != 0 {
		t.Fatal("expected the environment to be gone")
	}
}

func TestDeletingSomethingAbsentIsNotAnError(t *testing.T) {
	resp := do(t, testRouter(newFakeEnvironments()), "DELETE", "/environments/nope", "user-a", nil)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.Code)
	}
}

func TestDeleteOnAForeignEnvironmentLeavesItAlone(t *testing.T) {
	store := newFakeEnvironments()
	env := minimalEnvironment()
	env.Id, env.Owner = "env-1", "user-a"
	store.stored["env-1"] = env

	resp := do(t, testRouter(store), "DELETE", "/environments/env-1", "user-b", nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.Code)
	}
	if _, ok := store.stored["env-1"]; !ok {
		t.Fatal("expected the foreign environment to survive")
	}
}

func TestRequestsWithoutATokenAreRejected(t *testing.T) {
	router := testRouter(newFakeEnvironments())
	for _, call := range []struct {
		method string
		path   string
	}{
		{"GET", "/environments"},
		{"GET", "/environments/env-1"},
		{"PUT", "/environments/env-1"},
		{"POST", "/environments"},
		{"DELETE", "/environments/env-1"},
	} {
		resp := do(t, router, call.method, call.path, "", minimalEnvironment())
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("%s %s: expected 400 without a token, got %d", call.method, call.path, resp.Code)
		}
	}
}

func TestPostAssignsAnIdAndReturnsCreated(t *testing.T) {
	store := newFakeEnvironments()
	env := minimalEnvironment()
	env.Id = "ignored"
	resp := do(t, testRouter(store), "POST", "/environments", "user-a", env)
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.Code, resp.Body.String())
	}
	created := domain.Environment{}
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Id == "" || created.Id == "ignored" {
		t.Fatalf("expected a server assigned id, got %q", created.Id)
	}
}

func TestAStoreFailureDoesNotLeakItsMessage(t *testing.T) {
	store := newFakeEnvironments()
	store.failing = fmt.Errorf("server selection error: replica set has no primary at db-7.internal:27017")

	resp := do(t, testRouter(store), "GET", "/environments", "user-a", nil)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.Code)
	}
	if bytes.Contains(resp.Body.Bytes(), []byte("db-7.internal")) {
		t.Fatalf("internal topology must not reach the caller, got %s", resp.Body.String())
	}
}

func jsonEqual(t *testing.T, a interface{}, b interface{}) bool {
	t.Helper()
	rawA, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	rawB, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(rawA, rawB)
}

func TestATokenWithoutASubjectIsRejected(t *testing.T) {
	// a payload carrying no "sub" parses without error and yields an empty user
	// id. Stored as an owner it would match every other subjectless token, so
	// the boundary has to reject it rather than the access check. The shared
	// token library does not enforce this, which is why requireUser does.
	store := newFakeEnvironments()
	router := testRouter(store)

	subjectless := func() string {
		payload, err := json.Marshal(map[string]interface{}{"realm_access": map[string]interface{}{"roles": []string{"user"}}})
		if err != nil {
			t.Fatal(err)
		}
		encode := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
		return "Bearer " + encode([]byte(`{"alg":"none"}`)) + "." + encode(payload) + ".sig"
	}()

	for _, call := range []struct {
		method string
		path   string
	}{
		{"GET", "/environments"},
		{"GET", "/environments/env-1"},
		{"PUT", "/environments/env-1"},
		{"POST", "/environments"},
		{"DELETE", "/environments/env-1"},
	} {
		body, err := json.Marshal(minimalEnvironment())
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(call.method, call.path, bytes.NewReader(body))
		request.Header.Set("Authorization", subjectless)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: expected 401 for a token without a subject, got %d", call.method, call.path, recorder.Code)
		}
	}
	if len(store.stored) != 0 {
		t.Fatal("expected nothing to be stored for a subjectless token")
	}
}

// ---------------------------------------------------------------------------
// the trust boundary
//
// requireUser is where every handler reads the caller. These tests pin what it
// accepts and what it refuses. They moved here when the hand written lib/jwt was
// replaced by the shared service-commons token library: the behaviour they
// describe is the service's, not the library's, and it has to survive the next
// swap too.
// ---------------------------------------------------------------------------

// encodeTokenSegment encodes one jwt segment. Tokens use base64url without
// padding; the parser adds the padding back.
func encodeTokenSegment(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// tokenWithPayload builds `Bearer <header>.<payload>.<signature>` around a raw
// json payload. The signature is deliberately not a real one.
func tokenWithPayload(payload string) string {
	return "Bearer " + encodeTokenSegment(`{"alg":"RS256","typ":"JWT"}`) + "." + encodeTokenSegment(payload) + ".not-a-real-signature"
}

// doWithAuthorization sends a request with a verbatim Authorization header, so a
// test can hand in something no token builder would produce.
func doWithAuthorization(t *testing.T, router *gin.Engine, method string, path string, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(nil))
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// storeOwnedBy returns a store holding one environment owned by owner.
func storeOwnedBy(owner string) *fakeEnvironments {
	store := newFakeEnvironments()
	env := minimalEnvironment()
	env.Id, env.Owner = "env-1", owner
	store.stored["env-1"] = env
	return store
}

// TRUST BOUNDARY, BY DESIGN: the signature is never checked, because the api
// gateway checks it before a request reaches this service. A token whose
// signature is pure garbage is therefore accepted and owns its subject's data.
// Keep this test: it is the executable statement of where verification happens.
func TestATokenWithAGarbageSignatureIsAcceptedBecauseTheGatewayVerifies(t *testing.T) {
	garbage := "Bearer " + encodeTokenSegment(`{"alg":"RS256","typ":"JWT"}`) + "." +
		encodeTokenSegment(`{"sub":"user-a","realm_access":{"roles":["user"]}}`) + ".!!!!not-base64-at-all!!!!"

	response := doWithAuthorization(t, testRouter(storeOwnedBy("user-a")), "GET", "/environments", garbage)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	list := []domain.Environment{}
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Id != "env-1" {
		t.Fatalf("expected the unverified subject to own its data, got %v", list)
	}
}

// TRUST BOUNDARY, BY DESIGN: `exp` is never looked at either. The token in
// lib/test/helper expired in 2018 and the integration suite still runs on it.
func TestAnExpiredTokenIsAcceptedBecauseExpirationIsNotChecked(t *testing.T) {
	response := doWithAuthorization(t, testRouter(storeOwnedBy("user-a")), "GET", "/environments",
		tokenWithPayload(`{"sub":"user-a","exp":1,"realm_access":{"roles":["user"]}}`))
	if response.Code != http.StatusOK {
		t.Fatalf("expected the expired token to be accepted, got %d: %s", response.Code, response.Body.String())
	}
}

// The realm roles decide who is an admin, and mayAccess lets an admin through.
// This is the only place the role claim is read, and its shape changed with the
// library (a struct with a Roles field became a map), so it needs a test.
func TestAnAdminMayAccessAForeignEnvironment(t *testing.T) {
	admin := tokenWithPayload(`{"sub":"admin-user","realm_access":{"roles":["user","admin"]}}`)
	response := doWithAuthorization(t, testRouter(storeOwnedBy("user-a")), "GET", "/environments/env-1", admin)
	if response.Code != http.StatusOK {
		t.Fatalf("expected an admin to be let through, got %d: %s", response.Code, response.Body.String())
	}

	nonAdmin := tokenWithPayload(`{"sub":"other-user","realm_access":{"roles":["user","developer"]}}`)
	response = doWithAuthorization(t, testRouter(storeOwnedBy("user-a")), "GET", "/environments/env-1", nonAdmin)
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected a non admin to get 404, got %d: %s", response.Code, response.Body.String())
	}
}

// Untrusted input must never panic and must never turn into a 500. Every row is
// a request error, so every row is a 400.
func TestAMalformedTokenIsARequestErrorAndNeverPanics(t *testing.T) {
	validHeader := encodeTokenSegment(`{"alg":"RS256"}`)
	validPayload := encodeTokenSegment(`{"sub":"user-a"}`)

	tests := []struct {
		name          string
		authorization string
	}{
		{"missing Authorization header", ""},
		{"header that is only whitespace", " "},
		{"scheme without a token", "Bearer"},
		{"scheme with an empty token", "Bearer "},
		{"two spaces between scheme and token", "Bearer  " + validHeader + "." + validPayload + ".sig"},
		{"no dot separators", "Bearer " + validPayload},
		{"one dot separator", "Bearer " + validHeader + "." + validPayload},
		{"four dot separators", "Bearer " + validHeader + "." + validPayload + ".sig.extra"},
		{"only dots", "Bearer .."},
		{"header segment is not valid base64", "Bearer @@@." + validPayload + ".sig"},
		{"payload is not valid base64", "Bearer " + validHeader + ".@@not base64@@.sig"},
		{"payload is standard base64 with + and /", "Bearer " + validHeader + ".ab+/.sig"},
		{"payload is valid base64 but not json", "Bearer " + validHeader + "." + encodeTokenSegment("this is not json") + ".sig"},
		{"payload is a json array", "Bearer " + validHeader + "." + encodeTokenSegment(`["a","b"]`) + ".sig"},
		{"payload is a json string", "Bearer " + validHeader + "." + encodeTokenSegment(`"just-a-string"`) + ".sig"},
		{"payload is a json number", "Bearer " + validHeader + "." + encodeTokenSegment(`123`) + ".sig"},
		{"payload is truncated json", "Bearer " + validHeader + "." + encodeTokenSegment(`{"sub":`) + ".sig"},
		{"payload has the wrong type for sub", "Bearer " + validHeader + "." + encodeTokenSegment(`{"sub":{"nested":true}}`) + ".sig"},
		{"payload is empty", "Bearer " + validHeader + "..sig"},
		{"very long non base64 payload", "Bearer " + validHeader + "." + strings.Repeat("@", 1<<20) + ".sig"},
		{"very long invalid token without dots", "Bearer " + strings.Repeat("a", 1<<20)},
	}

	router := testRouter(newFakeEnvironments())
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// recovered here so a panic fails this subtest instead of tearing
			// down the whole binary
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("panicked on untrusted input: %v", recovered)
				}
			}()
			response := doWithAuthorization(t, router, "GET", "/environments", test.authorization)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", response.Code, response.Body.String())
			}
		})
	}
}

// A payload of json null parses without error and carries no subject, which is
// the 401 case and not the 400 case: the request was well formed, the caller is
// not identified.
func TestATokenWhosePayloadIsJsonNullIsRejectedAsSubjectless(t *testing.T) {
	response := doWithAuthorization(t, testRouter(newFakeEnvironments()), "GET", "/environments", tokenWithPayload(`null`))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", response.Code, response.Body.String())
	}
}

// What the shared token library accepts is not what the hand written lib/jwt
// accepted. None of these widen who owns what -- the subject still has to be
// there and the gateway still verifies -- but they are the observable difference
// between the two, so they are written down rather than discovered later.
func TestWhatTheSharedTokenLibraryChangedAboutTheAcceptedHeaderShape(t *testing.T) {
	validHeader := encodeTokenSegment(`{"alg":"RS256"}`)
	validPayload := encodeTokenSegment(`{"sub":"user-a","realm_access":{"roles":["user"]}}`)
	router := testRouter(storeOwnedBy("user-a"))

	tests := []struct {
		name          string
		authorization string
		expected      int
	}{
		// lib/jwt required "<scheme> <token>" and threw the scheme away. The
		// shared library strips a "bearer " prefix if there is one and otherwise
		// parses the whole header value.
		{"a bare token without a scheme is accepted now", validHeader + "." + validPayload + ".sig", http.StatusOK},
		{"a lower cased bearer prefix is accepted", "bearer " + validHeader + "." + validPayload + ".sig", http.StatusOK},
		{"a scheme other than bearer is refused now", "Basic " + validHeader + "." + validPayload + ".sig", http.StatusBadRequest},
		// the signature segment is never decoded, so trailing junk in it passes
		{"junk after the signature is accepted now", "Bearer " + validHeader + "." + validPayload + ".sig extra", http.StatusOK},
		// lib/jwt never looked at the header segment at all
		{"a malformed header segment is refused now", "Bearer @@@." + validPayload + ".sig", http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := doWithAuthorization(t, router, "GET", "/environments", test.authorization)
			if response.Code != test.expected {
				t.Fatalf("expected %d, got %d: %s", test.expected, response.Code, response.Body.String())
			}
		})
	}
}

// A client, or an HTTP/2 hop, may send the header name lower cased. This goes
// through a real server rather than building the header map by hand, because the
// canonicalisation that makes it work happens on the wire.
func TestALowerCasedAuthorizationHeaderSentOverTheWireIsRead(t *testing.T) {
	server := httptest.NewServer(testRouter(storeOwnedBy("user-a")))
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/environments", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header["authorization"] = []string{tokenWithPayload(`{"sub":"user-a","realm_access":{"roles":["user"]}}`)}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.StatusCode)
	}
	list := []domain.Environment{}
	if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected the caller to be identified, got %v", list)
	}
}

// Keycloak tokens with many roles get large. There is no length limit and there
// must be no quadratic behaviour either.
func TestAVeryLargeButWellFormedTokenIsAccepted(t *testing.T) {
	roles := make([]string, 5000)
	for index := range roles {
		roles[index] = "role-" + strings.Repeat("x", 40)
	}
	payload, err := json.Marshal(map[string]interface{}{
		"sub":          "user-a",
		"realm_access": map[string]interface{}{"roles": roles},
	})
	if err != nil {
		t.Fatal(err)
	}

	response := doWithAuthorization(t, testRouter(storeOwnedBy("user-a")), "GET", "/environments", tokenWithPayload(string(payload)))
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
}

// The integration suite in lib/test drives the real api over http with one
// keycloak issued token, and that token is the proof that the swap to the shared
// library preserved behaviour. It is checked here too, without docker: the token
// expired in 2018, its signature cannot be verified against anything this
// repository has, and it still has to identify its subject and its admin role.
func TestTheTokenTheIntegrationSuiteUsesStillIdentifiesItsSubjectAndRoles(t *testing.T) {
	const subject = "dd69ea0d-f553-4336-80f3-7f4567f85c7b"

	store := newFakeEnvironments()
	env := minimalEnvironment()
	env.Id, env.Owner = "env-1", subject
	store.stored["env-1"] = env
	router := testRouter(store)

	response := doWithAuthorization(t, router, "GET", "/environments", string(helper.AdminJwt))
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	list := []domain.Environment{}
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected the subject %q to own env-1, got %v", subject, list)
	}

	// the same token carries the admin realm role, which is what lets the legacy
	// template endpoints through
	foreign := newFakeEnvironments()
	other := minimalEnvironment()
	other.Id, other.Owner = "env-2", "somebody-else"
	foreign.stored["env-2"] = other
	response = doWithAuthorization(t, testRouter(foreign), "GET", "/environments/env-2", string(helper.AdminJwt))
	if response.Code != http.StatusOK {
		t.Fatalf("expected the admin role to be recognised, got %d: %s", response.Code, response.Body.String())
	}
}

// ---------------------------------------------------------------------------
// PATCH /environments/{id}/state - turning a boundary condition from outside
// ---------------------------------------------------------------------------

func TestPatchStateHandsTheChangeToTheRuntimeAndAnswers204(t *testing.T) {
	store := newFakeEnvironments()
	notifier := &recordingNotifier{}
	router := testRouterWithNotifier(store, notifier)
	if resp := do(t, router, "PUT", "/environments/env-1", "user-a", minimalEnvironment()); resp.Code != http.StatusOK {
		t.Fatalf("unable to create the environment: %d %s", resp.Code, resp.Body.String())
	}

	change := repo.StateChange{
		Context: map[string]interface{}{"outdoor_temperature": -7.5},
		Zones:   map[string]map[string]interface{}{"zone-1": {"temperature": 26.5}},
	}
	resp := do(t, router, "PATCH", "/environments/env-1/state", "user-a", change)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(notifier.changes) != 1 {
		t.Fatalf("expected exactly one change to reach the runtime, got %d", len(notifier.changes))
	}
	got := notifier.changes[0]
	if got.Context["outdoor_temperature"] != -7.5 || got.Zones["zone-1"]["temperature"] != 26.5 {
		t.Errorf("the change was not passed through unchanged: %#v", got)
	}
	//a state change is not a definition change, so nothing may be reloaded: a
	//reload would restart the tickers this change is meant to be read by
	if len(notifier.reloaded) != 1 {
		t.Errorf("expected only the reload of the PUT, got %v", notifier.reloaded)
	}
}

func TestPatchStateRefusesAnEmptyChange(t *testing.T) {
	store := newFakeEnvironments()
	notifier := &recordingNotifier{}
	router := testRouterWithNotifier(store, notifier)
	do(t, router, "PUT", "/environments/env-1", "user-a", minimalEnvironment())

	resp := do(t, router, "PATCH", "/environments/env-1/state", "user-a", repo.StateChange{})
	if resp.Code != http.StatusBadRequest {
		t.Errorf("an empty change does nothing and has to be refused, got %d", resp.Code)
	}
	if len(notifier.changes) != 0 {
		t.Errorf("nothing may reach the runtime, got %v", notifier.changes)
	}
}

func TestPatchStateOfSomebodyElsesEnvironmentIs404(t *testing.T) {
	store := newFakeEnvironments()
	notifier := &recordingNotifier{}
	router := testRouterWithNotifier(store, notifier)
	do(t, router, "PUT", "/environments/env-1", "user-a", minimalEnvironment())

	change := repo.StateChange{Context: map[string]interface{}{"outdoor_temperature": 30.0}}
	if resp := do(t, router, "PATCH", "/environments/env-1/state", "user-b", change); resp.Code != http.StatusNotFound {
		t.Errorf("expected 404 rather than 403, got %d", resp.Code)
	}
	if len(notifier.changes) != 0 {
		t.Errorf("a foreign environment must not be touched, got %v", notifier.changes)
	}
}

func TestPatchStateWithoutARuntimeIs404(t *testing.T) {
	store := newFakeEnvironments()
	//nil notifier: this instance serves the store only
	router := testRouter(store)
	do(t, router, "PUT", "/environments/env-1", "user-a", minimalEnvironment())

	change := repo.StateChange{Context: map[string]interface{}{"outdoor_temperature": 30.0}}
	resp := do(t, router, "PATCH", "/environments/env-1/state", "user-a", change)
	if resp.Code != http.StatusNotFound {
		t.Errorf("without a runtime the change cannot be applied and must not be claimed, got %d", resp.Code)
	}
}

func TestPatchStateReportsUnknownIdsAsABadRequest(t *testing.T) {
	store := newFakeEnvironments()
	notifier := &recordingNotifier{setErr: &repo.UnknownIdsError{Zones: []string{"no-zone"}}}
	router := testRouterWithNotifier(store, notifier)
	do(t, router, "PUT", "/environments/env-1", "user-a", minimalEnvironment())

	change := repo.StateChange{Zones: map[string]map[string]interface{}{"no-zone": {"temperature": 20.0}}}
	resp := do(t, router, "PATCH", "/environments/env-1/state", "user-a", change)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "no-zone") {
		t.Errorf("the offending id has to be named, got %q", resp.Body.String())
	}
}

func TestPatchStateOfAnEnvironmentThatIsNotRunningIs404(t *testing.T) {
	store := newFakeEnvironments()
	notifier := &recordingNotifier{setErr: repo.ErrNotRunning}
	router := testRouterWithNotifier(store, notifier)
	do(t, router, "PUT", "/environments/env-1", "user-a", minimalEnvironment())

	change := repo.StateChange{Context: map[string]interface{}{"outdoor_temperature": 30.0}}
	if resp := do(t, router, "PATCH", "/environments/env-1/state", "user-a", change); resp.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.Code)
	}
}

// ---------------------------------------------------------------------------
// An administrator sees every environment
// ---------------------------------------------------------------------------

// The list used to filter by owner unconditionally, while mayAccess already let
// an admin open any environment: what the detail route served was invisible in
// the list, and an environment of another user was in practice unfindable.
func TestAnAdminListsEveryEnvironment(t *testing.T) {
	store := newFakeEnvironments()
	router := testRouter(store)
	if resp := do(t, router, "PUT", "/environments/env-a", "user-a", minimalEnvironment()); resp.Code != http.StatusOK {
		t.Fatalf("setup failed: %d %s", resp.Code, resp.Body.String())
	}
	if resp := do(t, router, "PUT", "/environments/env-b", "user-b", minimalEnvironment()); resp.Code != http.StatusOK {
		t.Fatalf("setup failed: %d %s", resp.Code, resp.Body.String())
	}

	//a plain user still sees only their own
	resp := do(t, router, "GET", "/environments", "user-a", nil)
	if strings.Contains(resp.Body.String(), "env-b") {
		t.Errorf("a plain user must not see another user's environment: %s", resp.Body.String())
	}

	//the admin sees both, including one they do not own
	resp = doAsAdmin(t, router, "GET", "/environments", "user-c")
	if !strings.Contains(resp.Body.String(), "env-a") || !strings.Contains(resp.Body.String(), "env-b") {
		t.Errorf("an admin has to see every environment, got %s", resp.Body.String())
	}
}

func TestAnAdminOpensAndKeepsTheOwnerOfAForeignEnvironment(t *testing.T) {
	store := newFakeEnvironments()
	router := testRouter(store)
	if resp := do(t, router, "PUT", "/environments/env-a", "user-a", minimalEnvironment()); resp.Code != http.StatusOK {
		t.Fatalf("setup failed: %d", resp.Code)
	}

	if resp := doAsAdmin(t, router, "GET", "/environments/env-a", "admin-1"); resp.Code != http.StatusOK {
		t.Fatalf("an admin has to be able to open a foreign environment, got %d", resp.Code)
	}

	//editing it must not transfer ownership to the admin
	stored, err := store.Get(t.Context(), "env-a")
	if err != nil {
		t.Fatal(err)
	}
	stored.Name = "renamed by the admin"
	request := httptest.NewRequest("PUT", "/environments/env-a", bytes.NewReader(mustJson(t, stored)))
	request.Header.Set("Authorization", adminTokenFor("admin-1"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	after, err := store.Get(t.Context(), "env-a")
	if err != nil {
		t.Fatal(err)
	}
	if after.Owner != "user-a" {
		t.Errorf("an admin edit must not take ownership, owner is now %q", after.Owner)
	}
}

func mustJson(t *testing.T, value interface{}) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
