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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	permModel "github.com/SENERGY-Platform/permissions-v2/pkg/model"
	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// A caller that goes away mid-request. The fakes below fail on a cancelled
// context the way the real clients do, and cancel the request themselves at a
// chosen step, so a handler still on the request context stops right there.
// ---------------------------------------------------------------------------

// contextProbe counts the writes that arrived on a context without the bound a
// detached mutation must carry. Guarded, because a share writes concurrently.
type contextProbe struct {
	mux       sync.Mutex
	writes    int
	unbounded int
}

func (this *contextProbe) write(ctx context.Context) error {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.writes++
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > mutationTimeout {
		this.unbounded++
	}
	return ctx.Err()
}

func (this *contextProbe) assertBounded(t *testing.T, what string) {
	t.Helper()
	this.mux.Lock()
	defer this.mux.Unlock()
	if this.writes == 0 {
		t.Fatalf("%s: no write reached the fake, so the test proves nothing", what)
	}
	if this.unbounded != 0 {
		t.Errorf("%s: %d of %d writes ran on a context without the mutation deadline", what, this.unbounded, this.writes)
	}
}

// cancellingCatalog cancels the request after its first successful device call,
// or with cancelAfterCheck set, right after the platform check.
type cancellingCatalog struct {
	*fakeCatalog
	probe            contextProbe
	cancel           context.CancelFunc
	fired            bool
	cancelAfterCheck context.CancelFunc
}

func (this *cancellingCatalog) CheckReferences(ctx context.Context, token string, assets []devices.AssetReference) ([]domain.Problem, error) {
	problems, err := this.fakeCatalog.CheckReferences(ctx, token, assets)
	if this.cancelAfterCheck != nil {
		this.cancelAfterCheck()
	}
	return problems, err
}

func (this *cancellingCatalog) after() {
	if !this.fired && this.cancel != nil {
		this.fired = true
		this.cancel()
	}
}

func (this *cancellingCatalog) CreateDevice(ctx context.Context, token string, deviceTypeId string, name string) (devices.Device, error) {
	if err := this.probe.write(ctx); err != nil {
		return devices.Device{}, err
	}
	defer this.after()
	return this.fakeCatalog.CreateDevice(ctx, token, deviceTypeId, name)
}

func (this *cancellingCatalog) DeleteDevice(ctx context.Context, token string, id string) error {
	if err := this.probe.write(ctx); err != nil {
		return err
	}
	defer this.after()
	return this.fakeCatalog.DeleteDevice(ctx, token, id)
}

func (this *cancellingCatalog) RenameDevice(ctx context.Context, token string, id string, name string) error {
	if err := this.probe.write(ctx); err != nil {
		return err
	}
	defer this.after()
	return this.fakeCatalog.RenameDevice(ctx, token, id, name)
}

// contextEnvironments is the in-memory store failing on a cancelled context, as
// the mongodb driver does. afterGet and afterDelete run once the call succeeded.
type contextEnvironments struct {
	*fakeEnvironments
	probe       contextProbe
	afterGet    func()
	afterDelete func()
}

func (this *contextEnvironments) Get(ctx context.Context, id string) (domain.Environment, error) {
	if err := ctx.Err(); err != nil {
		return domain.Environment{}, err
	}
	env, err := this.fakeEnvironments.Get(ctx, id)
	if this.afterGet != nil {
		this.afterGet()
	}
	return env, err
}

func (this *contextEnvironments) Put(ctx context.Context, env domain.Environment) (int64, error) {
	if err := this.probe.write(ctx); err != nil {
		return 0, err
	}
	return this.fakeEnvironments.Put(ctx, env)
}

func (this *contextEnvironments) PutIfVersion(ctx context.Context, env domain.Environment, expected int64) (int64, error) {
	if err := this.probe.write(ctx); err != nil {
		return 0, err
	}
	return this.fakeEnvironments.PutIfVersion(ctx, env, expected)
}

func (this *contextEnvironments) Delete(ctx context.Context, id string) error {
	if err := this.probe.write(ctx); err != nil {
		return err
	}
	err := this.fakeEnvironments.Delete(ctx, id)
	if this.afterDelete != nil {
		this.afterDelete()
	}
	return err
}

// contextShares is the share store failing on a cancelled context. afterSave runs
// once a save succeeded.
type contextShares struct {
	*fakeShares
	probe     contextProbe
	afterSave func()
}

func (this *contextShares) Load(ctx context.Context, environmentId string) (repo.ShareSet, error) {
	if err := ctx.Err(); err != nil {
		return repo.ShareSet{}, err
	}
	return this.fakeShares.Load(ctx, environmentId)
}

func (this *contextShares) Save(ctx context.Context, set repo.ShareSet) (int64, error) {
	if err := this.probe.write(ctx); err != nil {
		return 0, err
	}
	version, err := this.fakeShares.Save(ctx, set)
	if err == nil && this.afterSave != nil {
		this.afterSave()
	}
	return version, err
}

func (this *contextShares) Delete(ctx context.Context, environmentId string) error {
	if err := this.probe.write(ctx); err != nil {
		return err
	}
	return this.fakeShares.Delete(ctx, environmentId)
}

// contextPermissions is permissions-v2 failing on a cancelled context, as the
// client in lib/permissions.go does.
type contextPermissions struct {
	*fakePermissions
	probe contextProbe
}

func (this *contextPermissions) GetResource(ctx context.Context, token string, topicId string, id string) (permModel.Resource, error, int) {
	if err := ctx.Err(); err != nil {
		return permModel.Resource{}, err, 0
	}
	return this.fakePermissions.GetResource(ctx, token, topicId, id)
}

func (this *contextPermissions) SetPermission(ctx context.Context, token string, topicId string, id string, rights permModel.ResourcePermissions) (permModel.ResourcePermissions, error, int) {
	if err := this.probe.write(ctx); err != nil {
		return permModel.ResourcePermissions{}, err, 0
	}
	return this.fakePermissions.SetPermission(ctx, token, topicId, id, rights)
}

// doWithin is do on a request carrying ctx, which is the context net/http
// cancels when the connection closes.
func doWithin(t *testing.T, ctx context.Context, router *gin.Engine, method string, path string, userId string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw := []byte{}
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(ctx)
	request.Header.Set("Authorization", tokenFor(userId))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func assertCancelled(t *testing.T, ctx context.Context) {
	t.Helper()
	if ctx.Err() == nil {
		t.Fatal("the request context was never cancelled, so the test proves nothing")
	}
}

func sortedDeviceIds(created []devices.Device) []string {
	result := []string{}
	for _, device := range created {
		result = append(result, device.Id)
	}
	sort.Strings(result)
	return result
}

// ---------------------------------------------------------------------------
// POST /environments
// ---------------------------------------------------------------------------

// Before the fix the create stopped after the first device: one device existed,
// no document named it, and the caller had no answer to recover it from.
func TestACreateWhoseCallerGoesAwayStillCreatesEveryDeviceAndStoresTheDocument(t *testing.T) {
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	catalog := &cancellingCatalog{fakeCatalog: &fakeCatalog{idsByName: namedDeviceIds()}, cancel: cancel}
	store := &contextEnvironments{fakeEnvironments: newFakeEnvironments()}
	mirror := newFakeGraphMirror()
	router := testRouterWith(store, catalog, mirror, nil)

	resp := doWithin(t, requestCtx, router, "POST", "/environments", "user-a", environmentWithTwoAssets())
	assertCancelled(t, requestCtx)
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.Code, resp.Body.String())
	}
	if got := sortedDeviceIds(catalog.created); len(got) != 2 || got[0] != "urn:device:kompressor" || got[1] != "urn:device:zaehler" {
		t.Fatalf("every device has to be created, got %v", got)
	}
	if len(store.stored) != 1 {
		t.Fatalf("the document has to be stored, the store holds %d", len(store.stored))
	}
	for _, env := range store.stored {
		if ref := assetNamed(t, env, "Kompressor 1").ExternalRef; ref != "urn:device:kompressor" {
			t.Errorf("the stored document has to name the first device, got %q", ref)
		}
		if ref := assetNamed(t, env, "Zähler").ExternalRef; ref != "urn:device:zaehler" {
			t.Errorf("the stored document has to name the second device, got %q", ref)
		}
		if env.ExternalGraphRef == "" || env.ExternalMeterGraphRef == "" || len(mirror.stored) != 2 {
			t.Errorf("both graphs have to be written and their refs stored, refs %q and %q, graphs %d",
				env.ExternalGraphRef, env.ExternalMeterGraphRef, len(mirror.stored))
		}
	}
	catalog.probe.assertBounded(t, "catalog")
	store.probe.assertBounded(t, "store")
}

// ---------------------------------------------------------------------------
// PUT /environments/{id}
// ---------------------------------------------------------------------------

// An update touches every kind of step: two devices to create, one to delete, one
// to rename, the graph, the document and the share inheritance. The caller goes
// away after the first device.
func TestAnUpdateWhoseCallerGoesAwayStillCompletesEveryStep(t *testing.T) {
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &contextEnvironments{fakeEnvironments: storeWithSharedEnvironment()}
	shares := &contextShares{fakeShares: newFakeShares()}
	shares.set("env-1", []string{"demo-user"}, nil)
	permissions := &contextPermissions{fakePermissions: newFakePermissions("dev-1", "dev-2", "dev-3", "urn:device:new-1", "urn:device:new-2")}
	permissions.ownGraph("urn:infai:ses:graph:1", "user-a")
	catalog := &cancellingCatalog{fakeCatalog: &fakeCatalog{idsByName: map[string]string{
		"Neue Maschine 1": "urn:device:new-1", "Neue Maschine 2": "urn:device:new-2",
	}}, cancel: cancel}
	mirror := newFakeGraphMirror()
	router := testRouterWithAll(store, shares, catalog, mirror, nil, permissions)

	sent := sharedEnvironment()
	sent.Zones[0].Assets[0].Name = "Hauptzähler Halle 1"
	//asset-2 goes, which releases dev-2
	sent.Zones[0].Assets = append([]domain.Asset{sent.Zones[0].Assets[0], sent.Zones[0].Assets[2]},
		newMachine("asset-4", "Neue Maschine 1"), newMachine("asset-5", "Neue Maschine 2"))

	resp := doWithin(t, requestCtx, router, "PUT", "/environments/env-1", "user-a", sent)
	assertCancelled(t, requestCtx)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if got := sortedDeviceIds(catalog.created); len(got) != 2 || got[0] != "urn:device:new-1" || got[1] != "urn:device:new-2" {
		t.Errorf("every new device has to be created, got %v", got)
	}
	stored := store.stored["env-1"]
	if stored.Version != 2 || len(stored.Zones[0].Assets) != 4 {
		t.Fatalf("the document has to be stored, version %d with %d assets", stored.Version, len(stored.Zones[0].Assets))
	}
	if ref := assetNamed(t, stored, "Neue Maschine 2").ExternalRef; ref != "urn:device:new-2" {
		t.Errorf("the stored document has to name the second new device, got %q", ref)
	}
	if stored.ExternalGraphRef != "urn:infai:ses:graph:1" {
		t.Errorf("the graph has to be written and its ref stored, got %q", stored.ExternalGraphRef)
	}
	if len(catalog.deleted) != 1 || catalog.deleted[0] != "dev-2" {
		t.Errorf("the released device has to be deleted, got %v", catalog.deleted)
	}
	if len(catalog.renamed) != 1 || catalog.renamed[0] != (renamedDevice{id: "dev-1", name: "Hauptzähler Halle 1"}) {
		t.Errorf("the renamed asset's device has to be renamed, got %+v", catalog.renamed)
	}
	for _, device := range []string{"urn:device:new-1", "urn:device:new-2"} {
		if rights := permissions.userRights(device, "demo-user"); !rights.Read {
			t.Errorf("%s has to inherit the share set, got %+v", device, rights)
		}
	}
	if rights := permissions.graphUserRights("urn:infai:ses:graph:1", "demo-user"); !rights.Read {
		t.Errorf("the created graph has to inherit the share set, got %+v", rights)
	}
	catalog.probe.assertBounded(t, "catalog")
	store.probe.assertBounded(t, "store")
	permissions.probe.assertBounded(t, "permissions")
}

// A put to an id that is new here clears a leftover share set as its first write,
// right after validation. A caller gone between validation and that write must
// not leave the set standing under a document that is then stored.
func TestAPutUnderANewIdWhoseCallerGoesAwayAfterValidationStillStartsUnshared(t *testing.T) {
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &contextEnvironments{fakeEnvironments: newFakeEnvironments()}
	shares := &contextShares{fakeShares: newFakeShares()}
	shares.set("env-9", []string{"former-user"}, nil)
	catalog := &cancellingCatalog{fakeCatalog: &fakeCatalog{idsByName: namedDeviceIds()}, cancelAfterCheck: cancel}
	router := testRouterWithAll(store, shares, catalog, nil, nil, nil)

	resp := doWithin(t, requestCtx, router, "PUT", "/environments/env-9", "user-a", environmentWithTwoAssets())
	assertCancelled(t, requestCtx)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if shares.has("env-9") {
		t.Errorf("the leftover set has to go, got users %v", shares.users("env-9"))
	}
	if len(catalog.created) != 2 {
		t.Errorf("every device has to be created, got %+v", catalog.created)
	}
	if _, ok := store.stored["env-9"]; !ok {
		t.Error("the document has to be stored")
	}
	shares.probe.assertBounded(t, "shares")
}

// Gone after the read and before validation finished, the put has written
// nothing yet, so it writes nothing at all and the leftover set stays with no
// document stored under it.
func TestAPutUnderANewIdWhoseCallerGoesAwayAfterTheReadWritesNothing(t *testing.T) {
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &contextEnvironments{fakeEnvironments: newFakeEnvironments(), afterGet: cancel}
	shares := &contextShares{fakeShares: newFakeShares()}
	shares.set("env-9", []string{"former-user"}, nil)
	catalog := &cancellingCatalog{fakeCatalog: &fakeCatalog{idsByName: namedDeviceIds()}}
	router := testRouterWithAll(store, shares, catalog, nil, nil, nil)

	resp := doWithin(t, requestCtx, router, "PUT", "/environments/env-9", "user-a", environmentWithTwoAssets())
	assertCancelled(t, requestCtx)
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("expected the check to fail with 502, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(catalog.checked) != 1 {
		t.Fatalf("the check has to have run, so this proves the order, got %d checks", len(catalog.checked))
	}
	if got := shares.users("env-9"); len(got) != 1 || got[0] != "former-user" {
		t.Errorf("nothing was stored, so the set has to stay, got %v", got)
	}
	if len(catalog.created) != 0 || len(store.stored) != 0 || shares.probe.writes != 0 {
		t.Errorf("nothing may be written, got devices %+v, documents %d, share writes %d",
			catalog.created, len(store.stored), shares.probe.writes)
	}
}

// ---------------------------------------------------------------------------
// DELETE /environments/{id}
// ---------------------------------------------------------------------------

// Before the fix a delete that lost its caller left the document gone and the
// devices, the graph and the share set standing, with nothing left to find them.
func TestADeleteWhoseCallerGoesAwayStillDeletesEveryDevice(t *testing.T) {
	for _, point := range []string{"after the document", "after the first device"} {
		t.Run(point, func(t *testing.T) {
			requestCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			env := sharedEnvironment()
			env.ExternalGraphRef = "urn:infai:ses:graph:7"
			store := &contextEnvironments{fakeEnvironments: newFakeEnvironments()}
			store.stored["env-1"] = env
			shares := &contextShares{fakeShares: newFakeShares()}
			shares.set("env-1", []string{"demo-user"}, nil)
			catalog := &cancellingCatalog{fakeCatalog: &fakeCatalog{}}
			if point == "after the document" {
				store.afterDelete = cancel
			} else {
				catalog.cancel = cancel
			}
			mirror := newFakeGraphMirror()
			router := testRouterWithAll(store, shares, catalog, mirror, nil, nil)

			resp := doWithin(t, requestCtx, router, "DELETE", "/environments/env-1", "user-a", nil)
			assertCancelled(t, requestCtx)
			if resp.Code != http.StatusNoContent {
				t.Fatalf("expected 204, got %d: %s", resp.Code, resp.Body.String())
			}
			if len(store.stored) != 0 {
				t.Error("the document has to be gone")
			}
			deleted := append([]string{}, catalog.deleted...)
			sort.Strings(deleted)
			if len(deleted) != 2 || deleted[0] != "dev-1" || deleted[1] != "dev-2" {
				t.Errorf("every managed device has to be deleted and the picked one kept, got %v", deleted)
			}
			if len(mirror.deleted) != 1 || mirror.deleted[0] != "urn:infai:ses:graph:7" {
				t.Errorf("the graph has to be deleted, got %v", mirror.deleted)
			}
			if shares.has("env-1") {
				t.Error("the share set has to be deleted")
			}
			catalog.probe.assertBounded(t, "catalog")
			store.probe.assertBounded(t, "store")
			shares.probe.assertBounded(t, "shares")
		})
	}
}

// ---------------------------------------------------------------------------
// PUT /environments/{id}/shares
// ---------------------------------------------------------------------------

// The union is stored first and the requested set once every resource went
// through. A caller gone right after the union must still get the rights written
// and the set shrunk to what was asked for.
func TestAShareWhoseCallerGoesAwayAfterTheUnionStillWritesEveryRightAndTheFinalSet(t *testing.T) {
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &contextEnvironments{fakeEnvironments: storeWithSharedEnvironment()}
	shares := &contextShares{fakeShares: newFakeShares(), afterSave: cancel}
	shares.set("env-1", []string{"former-user"}, nil)
	permissions := &contextPermissions{fakePermissions: newFakePermissions("dev-1", "dev-2", "dev-3")}
	router := testRouterWithShares(store, shares, nil, permissions)

	resp := doWithin(t, requestCtx, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}})
	assertCancelled(t, requestCtx)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	for _, device := range []string{"dev-1", "dev-2"} {
		if rights := permissions.userRights(device, "demo-user"); !rights.Read || !rights.Execute {
			t.Errorf("%s: the share has to be written, got %+v", device, rights)
		}
	}
	if got := shares.users("env-1"); len(got) != 1 || got[0] != "demo-user" {
		t.Errorf("the requested set has to replace the union, got %v", got)
	}
	shares.probe.assertBounded(t, "shares")
	permissions.probe.assertBounded(t, "permissions")
}
