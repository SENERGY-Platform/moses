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
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/gin-gonic/gin"
)

// validateWitnesses are fakes for everything a create writes to, so a test can
// see that validation wrote to none of them.
type validateWitnesses struct {
	store       *fakeEnvironments
	shares      *fakeShares
	catalog     *fakeCatalog
	mirror      *fakeGraphMirror
	notifier    *recordingNotifier
	permissions *fakePermissions
	router      *gin.Engine
}

func newValidateWitnesses() validateWitnesses {
	w := validateWitnesses{
		store:       newFakeEnvironments(),
		shares:      newFakeShares(),
		catalog:     &fakeCatalog{},
		mirror:      newFakeGraphMirror(),
		notifier:    &recordingNotifier{},
		permissions: newFakePermissions(),
	}
	w.router = testRouterWithAll(w.store, w.shares, w.catalog, w.mirror, w.notifier, w.permissions)
	return w
}

// wrote lists every write any collaborator received; empty means none.
func (this validateWitnesses) wrote() []string {
	result := []string{}
	add := func(what string, count int) {
		if count > 0 {
			result = append(result, what)
		}
	}
	add("environment store", len(this.store.stored))
	add("share store save", len(this.shares.saves))
	add("share store delete", len(this.shares.deletes))
	add("device created", len(this.catalog.created))
	add("device deleted", len(this.catalog.deleted))
	add("device renamed", len(this.catalog.renamed))
	add("graph written", len(this.mirror.sent))
	add("graph deleted", len(this.mirror.deleted))
	add("permission written", len(this.permissions.sets))
	add("runtime reloaded", len(this.notifier.reloaded))
	add("runtime removed", len(this.notifier.removed))
	add("runtime state changed", len(this.notifier.changes))
	return result
}

// invalidEnvironment breaks several unrelated rules at once, script checks
// included, so the whole problem list is compared rather than one message.
func invalidEnvironment() domain.Environment {
	env := minimalEnvironment()
	env.Name = ""
	env.Zones[0].Type = "nonsense"
	twin := env.Zones[0].Assets[0]
	env.Zones[0].Assets[0].Id = "asset-1"
	twin.Id = "asset-1"
	twin.Name = "Zwilling"
	twin.Channels = []domain.Channel{{
		Name: "Tief", Direction: domain.Sensor, IntervalSeconds: 30,
		Source: domain.Source{Kind: domain.SourceScript, Script: &domain.ScriptSource{
			Code: strings.Repeat("(", 300) + "1" + strings.Repeat(")", 300),
		}},
	}}
	env.Zones[0].Assets = append(env.Zones[0].Assets, twin)
	return env
}

func TestValidateAcceptsAValidDocumentAndWritesNothing(t *testing.T) {
	w := newValidateWitnesses()
	//a new asset with a device type: a create would provision its device
	env := environmentWithNewMachine()

	resp := do(t, w.router, http.MethodPost, "/environments/validate", "user-a", env)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if got := resp.Body.String(); got != `{"valid":true}` {
		t.Errorf("expected {\"valid\":true}, got %s", got)
	}
	if wrote := w.wrote(); len(wrote) != 0 {
		t.Fatalf("validate must write nothing, but wrote to: %v", wrote)
	}

	//the witnesses do see a write: the same document created for real touches them
	if resp := do(t, w.router, http.MethodPost, "/environments", "user-a", env); resp.Code != http.StatusCreated {
		t.Fatalf("expected 201 for the create, got %d: %s", resp.Code, resp.Body.String())
	}
	for _, expected := range []string{"environment store", "share store delete", "device created", "graph written", "runtime reloaded"} {
		found := false
		for _, wrote := range w.wrote() {
			found = found || wrote == expected
		}
		if !found {
			t.Errorf("the create was expected to reach %q, which leaves the witness unproven; saw %v", expected, w.wrote())
		}
	}
}

func TestValidateRefusesWithTheBodyPostAndPutGiveAndWritesNothing(t *testing.T) {
	w := newValidateWitnesses()
	env := invalidEnvironment()

	validated := do(t, w.router, http.MethodPost, "/environments/validate", "user-a", env)
	if validated.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", validated.Code, validated.Body.String())
	}
	if wrote := w.wrote(); len(wrote) != 0 {
		t.Fatalf("a refused validate must write nothing, but wrote to: %v", wrote)
	}
	problems := domain.ValidationError{}
	if err := json.Unmarshal(validated.Body.Bytes(), &problems); err != nil {
		t.Fatalf("expected the problem list, got %s", validated.Body.String())
	}
	for _, path := range []string{"name", "zones[0].type", "zones[0].assets[1].id", "zones[0].assets[1].channels[0].source.script.code"} {
		found := false
		for _, problem := range problems.Problems {
			found = found || problem.Path == path
		}
		if !found {
			t.Errorf("expected a problem at %s, got %+v", path, problems.Problems)
		}
	}

	posted := do(t, w.router, http.MethodPost, "/environments", "user-a", env)
	if posted.Code != validated.Code || posted.Body.String() != validated.Body.String() {
		t.Errorf("validate and POST answer differently:\nvalidate %d %s\npost     %d %s",
			validated.Code, validated.Body.String(), posted.Code, posted.Body.String())
	}
	put := do(t, w.router, http.MethodPut, "/environments/env-1", "user-a", env)
	if put.Code != validated.Code || put.Body.String() != validated.Body.String() {
		t.Errorf("validate and PUT answer differently:\nvalidate %d %s\nput      %d %s",
			validated.Code, validated.Body.String(), put.Code, put.Body.String())
	}
}

// An unreadable body is refused before validation, with the message of POST.
func TestValidateRefusesAnUnreadableBodyLikePost(t *testing.T) {
	w := newValidateWitnesses()
	for _, body := range []string{"{", `{"zones": 5}`, ""} {
		validated := sendRaw(w.router, http.MethodPost, "/environments/validate", body)
		posted := sendRaw(w.router, http.MethodPost, "/environments", body)
		if validated.Code != http.StatusBadRequest {
			t.Errorf("%q: expected 400, got %d: %s", body, validated.Code, validated.Body.String())
		}
		if posted.Code != validated.Code || posted.Body.String() != validated.Body.String() {
			t.Errorf("%q: validate and POST answer differently:\nvalidate %d %s\npost     %d %s",
				body, validated.Code, validated.Body.String(), posted.Code, posted.Body.String())
		}
	}
	if wrote := w.wrote(); len(wrote) != 0 {
		t.Fatalf("an unreadable body must write nothing, but wrote to: %v", wrote)
	}
}

// POST replaces the id in the body, so that id must not collide with a nested
// one during validation either.
func TestValidateIgnoresTheIdInTheBodyLikePost(t *testing.T) {
	w := newValidateWitnesses()
	env := minimalEnvironment()
	env.Id = "shared-id"
	env.Zones[0].Id = "shared-id"

	resp := do(t, w.router, http.MethodPost, "/environments/validate", "user-a", env)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 as POST gives 201, got %d: %s", resp.Code, resp.Body.String())
	}
	if resp := do(t, w.router, http.MethodPost, "/environments", "user-a", env); resp.Code != http.StatusCreated {
		t.Fatalf("the create was expected to succeed, got %d: %s", resp.Code, resp.Body.String())
	}
}

// validate is a static segment beside /environments/:id: it must reach its own
// handler, and an environment stored under the id "validate" keeps its routes.
func TestTheValidateRouteAndAnEnvironmentNamedValidateDoNotShadowEachOther(t *testing.T) {
	w := newValidateWitnesses()
	if resp := do(t, w.router, http.MethodPut, "/environments/validate", "user-a", minimalEnvironment()); resp.Code != http.StatusOK {
		t.Fatalf("expected PUT /environments/validate to store, got %d: %s", resp.Code, resp.Body.String())
	}
	stored := w.store.stored["validate"]
	if stored.Version != 1 {
		t.Fatalf("expected the environment validate at version 1, got %+v", stored)
	}

	other := minimalEnvironment()
	other.Name = "Anderer Standort"
	resp := do(t, w.router, http.MethodPost, "/environments/validate", "user-a", other)
	if resp.Code != http.StatusOK || resp.Body.String() != `{"valid":true}` {
		t.Fatalf("expected the validate handler, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(w.store.stored) != 1 || w.store.stored["validate"].Version != 1 || w.store.stored["validate"].Name != stored.Name {
		t.Fatalf("validate touched the environment named validate: %+v", w.store.stored)
	}

	if resp := do(t, w.router, http.MethodGet, "/environments/validate", "user-a", nil); resp.Code != http.StatusOK {
		t.Errorf("GET /environments/validate: expected the stored environment, got %d: %s", resp.Code, resp.Body.String())
	}
	if resp := do(t, w.router, http.MethodPost, "/environments/validate/backfill", "user-a", backfillBody()); resp.Code != http.StatusAccepted {
		t.Errorf("POST /environments/validate/backfill: expected 202, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(w.notifier.backfills) != 1 || w.notifier.backfills[0].EnvironmentId != "validate" {
		t.Errorf("expected a backfill of the environment validate, got %+v", w.notifier.backfills)
	}
	if resp := do(t, w.router, http.MethodPost, "/environments/validate/history", "user-a", historyBody()); resp.Code != http.StatusAccepted {
		t.Errorf("POST /environments/validate/history: expected 202, got %d: %s", resp.Code, resp.Body.String())
	}
	if resp := do(t, w.router, http.MethodDelete, "/environments/validate", "user-a", nil); resp.Code != http.StatusNoContent {
		t.Errorf("DELETE /environments/validate: expected 204, got %d: %s", resp.Code, resp.Body.String())
	}
	if _, still := w.store.stored["validate"]; still {
		t.Error("DELETE /environments/validate did not delete the environment")
	}
}
