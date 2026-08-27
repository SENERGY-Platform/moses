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
)

// ---------------------------------------------------------------------------
// The optimistic lock on PUT /environments/{id}. What it is for is the gap in
// docs/device-lifecycle-of-assets.md: of two concurrent updates the loser's
// device cleanup could delete a device the winning document still publishes
// through. So the interesting assertion is not the status code - it is that a
// refused write touched nothing outside the store.
// ---------------------------------------------------------------------------

func TestTheAnswerOfEveryWriteCarriesTheNewVersion(t *testing.T) {
	store := newFakeEnvironments()
	router := testRouter(store)

	created := domain.Environment{}
	resp := do(t, router, "POST", "/environments", "user-a", minimalEnvironment())
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.Code, resp.Body.String())
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Version != 1 {
		t.Errorf("a created environment starts at version 1, got %d", created.Version)
	}

	updated := putEnvironment(t, router, created.Id, "user-a", created)
	if updated.Version != 2 {
		t.Errorf("expected version 2 after the first update, got %d", updated.Version)
	}
	again := putEnvironment(t, router, created.Id, "user-a", updated)
	if again.Version != 3 {
		t.Errorf("expected version 3 after the second update, got %d", again.Version)
	}
	if store.stored[created.Id].Version != 3 {
		t.Errorf("expected the stored document to be at version 3, got %d", store.stored[created.Id].Version)
	}
}

// A version in the body of a POST is as meaningless as an id in it: the id is
// assigned here and nobody else knows it yet.
func TestPostIgnoresAVersionInTheBody(t *testing.T) {
	store := newFakeEnvironments()
	env := minimalEnvironment()
	env.Version = 4711

	resp := do(t, testRouter(store), "POST", "/environments", "user-a", env)
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.Code, resp.Body.String())
	}
	created := domain.Environment{}
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Version != 1 {
		t.Errorf("expected the created environment to start at version 1, got %d", created.Version)
	}
	if store.stored[created.Id].Version != 1 {
		t.Errorf("expected the stored version to be 1, got %d", store.stored[created.Id].Version)
	}
}

// The backwards compatible path: a client that knows nothing of the field sends
// no version, keeps writing, and the count still runs - so a client that does
// send one is protected against it.
func TestAClientWithoutAVersionKeepsWritingAndTheVersionStillGrows(t *testing.T) {
	store := newFakeEnvironments()
	router := testRouter(store)

	for run := int64(1); run <= 3; run++ {
		//the same body every time, always without a version, exactly as a client
		//written before the field existed sends it
		result := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())
		if result.Version != run {
			t.Errorf("expected version %d on write %d, got %d", run, run, result.Version)
		}
	}
}

func TestPutRefusesAWriteAgainstAnOutdatedVersion(t *testing.T) {
	store := newFakeEnvironments()
	router := testRouter(store)

	read := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())
	//somebody else stores a change in between
	winner := read
	winner.Name = "written by the winner"
	putEnvironment(t, router, "env-1", "user-a", winner)

	//and the first editor writes back the document it read
	loser := read
	loser.Name = "written by the loser"
	resp := do(t, router, "PUT", "/environments/env-1", "user-a", loser)

	if resp.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", resp.Code, resp.Body.String())
	}
	//both versions in the message: reloading is the only useful reaction, and a
	//caller that cannot see how far behind it was cannot tell stale from broken
	body := resp.Body.String()
	if !strings.Contains(body, "version 1") || !strings.Contains(body, "stored is 2") {
		t.Errorf("expected the message to name both versions, got %q", body)
	}
	if stored := store.stored["env-1"]; stored.Name != "written by the winner" || stored.Version != 2 {
		t.Errorf("the refused write must have changed nothing, stored is %q at version %d", stored.Name, stored.Version)
	}
}

// The assertion the whole feature exists for: a refused write must not have
// created a device, rebuilt a graph or - the damaging one - deleted a device
// the winning document still references.
func TestARefusedWriteHasNoSideEffectsOutsideTheStore(t *testing.T) {
	store := newFakeEnvironments()
	catalog := &fakeCatalog{idsByName: map[string]string{
		"Hauptzähler Strom": "urn:device:meter",
		"Neuer Zähler":      "urn:device:new-meter",
	}}
	mirror := newFakeGraphMirror()
	notifier := &recordingNotifier{}
	router := testRouterWith(store, catalog, mirror, notifier)

	read := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())
	//somebody else wins the race
	winner := read
	winner.Name = "written by the winner"
	putEnvironment(t, router, "env-1", "user-a", winner)

	created := len(catalog.created)
	deleted := len(catalog.deleted)
	mirrored := len(mirror.sent)
	reloads := len(notifier.reloaded)

	//the loser drops the asset - the change that would make the cleanup delete
	//the device the winning document still publishes through - and adds one that
	//would have to be provisioned
	loser := read
	loser.Zones[0].Assets[0] = domain.Asset{
		Name:           "Neuer Zähler",
		Kind:           domain.AssetMeter,
		ExternalTypeId: "urn:infai:ses:device-type:abc",
		Channels:       loser.Zones[0].Assets[0].Channels,
	}
	resp := do(t, router, "PUT", "/environments/env-1", "user-a", loser)

	if resp.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(catalog.created) != created {
		t.Errorf("a refused write must not create a device, got %#v", catalog.created[created:])
	}
	if len(catalog.deleted) != deleted {
		t.Errorf("a refused write must not delete a device, got %#v", catalog.deleted[deleted:])
	}
	if len(mirror.sent) != mirrored {
		t.Errorf("a refused write must not rewrite the graph, got %#v", mirror.sent[mirrored:])
	}
	if len(notifier.reloaded) != reloads {
		t.Errorf("a refused write must not reload the runtime, got %#v", notifier.reloaded[reloads:])
	}
}

// The conflict the handler's own check cannot see: the winning write lands
// between the read and the write. The store refuses it, and the cleanup - the
// step that deletes devices - is on the far side of that refusal.
func TestAConflictDecidedByTheStoreStillRunsNoCleanup(t *testing.T) {
	store := newFakeEnvironments()
	catalog := &fakeCatalog{idsByName: map[string]string{"Hauptzähler Strom": "urn:device:meter"}}
	notifier := &recordingNotifier{}
	router := testRouterWith(store, catalog, nil, notifier)

	read := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())
	deleted := len(catalog.deleted)
	reloads := len(notifier.reloaded)

	//a competing write lands after the handler read the document and compared
	//the version, which is the window its own check cannot cover
	store.beforeWrite = func() {
		store.beforeWrite = nil
		winner := store.stored["env-1"]
		winner.Name = "written by the winner"
		store.write(winner)
	}

	loser := read
	loser.Zones[0].Assets = nil
	resp := do(t, router, "PUT", "/environments/env-1", "user-a", loser)

	if resp.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(catalog.deleted) != deleted {
		t.Errorf("a write refused by the store must not delete a device, got %#v", catalog.deleted[deleted:])
	}
	if len(notifier.reloaded) != reloads {
		t.Errorf("a write refused by the store must not reload the runtime, got %#v", notifier.reloaded[reloads:])
	}
	if stored := store.stored["env-1"]; stored.Name != "written by the winner" {
		t.Errorf("the refused write must not have landed, stored is %q", stored.Name)
	}
}

// Putting an export under a new id is how a document is copied, and an export
// carries the version of the original. Refusing that would break the copy.
func TestAVersionCarriedAgainstADocumentThatDoesNotExistCreatesIt(t *testing.T) {
	store := newFakeEnvironments()
	router := testRouter(store)

	original := putEnvironment(t, router, "original", "user-a", minimalEnvironment())
	for run := 0; run < 3; run++ {
		original = putEnvironment(t, router, "original", "user-a", original)
	}
	if original.Version < 2 {
		t.Fatalf("expected the original to have moved past version 1, got %d", original.Version)
	}

	copied := putEnvironment(t, router, "copy", "user-a", original)
	if copied.Version != 1 {
		t.Errorf("a copy is a new document and starts at version 1, got %d", copied.Version)
	}
	if _, ok := store.stored["copy"]; !ok {
		t.Error("expected the copy to be stored")
	}
}

// A document stored before the version field existed reads as version 0. A
// caller that nevertheless carries one is refused, and told the stored version
// is 0 rather than that the document disappeared - it is right there.
func TestAConflictAgainstADocumentWithoutAVersionNamesZeroRatherThanGone(t *testing.T) {
	store := newFakeEnvironments()
	router := testRouter(store)
	stored := storeDirectly(t, store, "env-old", "user-a", minimalEnvironment())
	if stored.Version != 0 {
		t.Fatalf("the precondition is a document at version 0, got %d", stored.Version)
	}

	stale := stored
	stale.Version = 3
	resp := do(t, router, "PUT", "/environments/env-old", "user-a", stale)
	if resp.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, "stored is 0") {
		t.Errorf("expected the message to name the stored version 0, got %q", body)
	}
	if strings.Contains(body, "no longer exists") {
		t.Errorf("the document is stored, so the message must not say it is gone, got %q", body)
	}
}

// A conflict is decided before ownership is: a caller without access must not
// be able to tell an outdated version from a document that is not theirs.
func TestAConflictIsNotReportedToACallerWithoutAccess(t *testing.T) {
	store := newFakeEnvironments()
	router := testRouter(store)
	read := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())

	stale := read
	stale.Version = 4711
	resp := do(t, router, "PUT", "/environments/env-1", "user-b", stale)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a caller without access, got %d: %s", resp.Code, resp.Body.String())
	}
}
