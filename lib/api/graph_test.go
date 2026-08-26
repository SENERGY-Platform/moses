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
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/graphs"
)

// ---------------------------------------------------------------------------
// The graph is a mirror the server owns: which graph an environment writes into
// is decided from what is stored, never from what the client sent back. The
// expensive case is the copy - an export put under a new id still carries the
// ref of the document it was copied from, and honouring it would have the copy
// overwrite and later delete the original's graph.
// ---------------------------------------------------------------------------

// fakeGraphMirror is an in-memory graph api with the semantics the real one has:
// an empty id creates and assigns one, a set id replaces.
type fakeGraphMirror struct {
	stored map[string]models.Graph
	// sent records every graph as it arrived, so a test can assert on the id it
	// was addressed to rather than only on the result
	sent       []models.Graph
	deleted    []string
	tokensSeen []string
	created    int

	setErr     error
	setCode    int
	deleteErr  error
	deleteCode int
}

func newFakeGraphMirror() *fakeGraphMirror {
	return &fakeGraphMirror{stored: map[string]models.Graph{}}
}

func (this *fakeGraphMirror) SetGraph(token string, graph models.Graph) (models.Graph, error, int) {
	this.tokensSeen = append(this.tokensSeen, token)
	this.sent = append(this.sent, graph)
	if this.setErr != nil {
		return models.Graph{}, this.setErr, this.setCode
	}
	if graph.Id == "" {
		this.created++
		graph.Id = fmt.Sprintf("urn:infai:ses:graph:%d", this.created)
	}
	this.stored[graph.Id] = graph
	return graph, nil, http.StatusOK
}

func (this *fakeGraphMirror) DeleteGraph(token string, id string) (error, int) {
	this.tokensSeen = append(this.tokensSeen, token)
	this.deleted = append(this.deleted, id)
	if this.deleteErr != nil {
		return this.deleteErr, this.deleteCode
	}
	delete(this.stored, id)
	return nil, http.StatusOK
}

func (this *fakeGraphMirror) lastSent(t *testing.T) models.Graph {
	t.Helper()
	if len(this.sent) == 0 {
		t.Fatal("nothing was mirrored at all")
	}
	return this.sent[len(this.sent)-1]
}

func graphName(graph models.Graph) string {
	for _, node := range graph.Nodes {
		if node.Id != graphs.RootNodeId {
			continue
		}
		for _, attr := range node.Attributes {
			if attr.Key == graphs.NameAttribute {
				return attr.Value
			}
		}
	}
	return ""
}

// A created environment gets a graph, and the id it got is stored with the
// document in the same write - the way back to the mirror on every later save.
func TestCreatingAnEnvironmentMirrorsItAsAGraph(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)

	resp := do(t, router, "POST", "/environments", "user-a", minimalEnvironment())
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.Code, resp.Body.String())
	}
	created := domain.Environment{}
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	if created.ExternalGraphRef != "urn:infai:ses:graph:1" {
		t.Fatalf("expected the assigned graph ref in the answer, got %q", created.ExternalGraphRef)
	}
	if stored := store.stored[created.Id].ExternalGraphRef; stored != created.ExternalGraphRef {
		t.Errorf("the ref has to be in the stored document, not only in the answer, got %q", stored)
	}
	if id := mirror.lastSent(t).Id; id != "" {
		t.Errorf("a graph that does not exist yet is created without an id, got %q", id)
	}
	if graphName(mirror.stored["urn:infai:ses:graph:1"]) != "Metallbau Musterstadt" {
		t.Errorf("expected the environment name on the mirrored graph, got %+v", mirror.stored)
	}
	if len(mirror.tokensSeen) == 0 || mirror.tokensSeen[0] != tokenFor("user-a") {
		t.Errorf("the graph is written with the caller's own token, got %v", mirror.tokensSeen)
	}
}

// The whole document is sent on every update, so the ref it carries is worth
// nothing: what is stored decides which graph is written.
func TestUpdatingAnEnvironmentWritesTheStoredGraphNotTheSentOne(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)

	stored := storeDirectly(t, store, "env-1", "user-a", minimalEnvironment())
	stored.ExternalGraphRef = "urn:infai:ses:graph:owned"
	store.stored["env-1"] = stored

	//the client echoes a different ref, whether from a stale copy or by hand
	sending := copyEnvironment(t, stored)
	sending.ExternalGraphRef = "urn:infai:ses:graph:somebody-else"
	sending.Name = "Metallbau Musterstadt, neu"
	answer := putEnvironment(t, router, "env-1", "user-a", sending)

	if answer.ExternalGraphRef != "urn:infai:ses:graph:owned" {
		t.Errorf("expected the stored ref to survive, got %q", answer.ExternalGraphRef)
	}
	if id := mirror.lastSent(t).Id; id != "urn:infai:ses:graph:owned" {
		t.Fatalf("expected the update to address the stored graph, it addressed %q", id)
	}
	if _, touched := mirror.stored["urn:infai:ses:graph:somebody-else"]; touched {
		t.Error("the graph named by the client must not be written")
	}
	if graphName(mirror.stored["urn:infai:ses:graph:owned"]) != "Metallbau Musterstadt, neu" {
		t.Error("the mirror was not brought up to date with the new document")
	}
}

// The dangerous one: an export put under a new id is a copy, and its ref still
// points at the graph of the original. A copy owns nothing.
func TestACopyUnderANewIdGetsItsOwnGraphAndLeavesTheOriginalAlone(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)

	original := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())
	if original.ExternalGraphRef == "" {
		t.Fatal("setup: the original was not mirrored")
	}
	originalGraph := mirror.stored[original.ExternalGraphRef]

	//the export of the original, put under a fresh id, unedited
	copied := copyEnvironment(t, original)
	copied.Name = "Metallbau Zweitwerk"
	answer := putEnvironment(t, router, "env-2", "user-a", copied)

	if answer.ExternalGraphRef == original.ExternalGraphRef {
		t.Fatalf("the copy took the graph of the original: both are %q", answer.ExternalGraphRef)
	}
	if answer.ExternalGraphRef == "" {
		t.Fatal("the copy was not mirrored at all")
	}
	if got := graphName(mirror.stored[original.ExternalGraphRef]); got != graphName(originalGraph) {
		t.Errorf("the graph of the original was overwritten, its name is now %q", got)
	}
	if got := graphName(mirror.stored[answer.ExternalGraphRef]); got != "Metallbau Zweitwerk" {
		t.Errorf("expected the copy's own graph to carry its name, got %q", got)
	}

	//and deleting the copy leaves the original's graph standing
	if resp := do(t, router, "DELETE", "/environments/env-2", "user-a", nil); resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", resp.Code, resp.Body.String())
	}
	if _, alive := mirror.stored[original.ExternalGraphRef]; !alive {
		t.Error("deleting the copy deleted the graph of the original")
	}
}

// Deleting an environment deletes its mirror: a graph nobody can reach from an
// environment is a location that no longer exists.
func TestDeletingAnEnvironmentDeletesItsGraph(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)

	created := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())

	if resp := do(t, router, "DELETE", "/environments/env-1", "user-a", nil); resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(mirror.deleted) != 1 || mirror.deleted[0] != created.ExternalGraphRef {
		t.Fatalf("expected exactly the graph of this environment to be deleted, got %v", mirror.deleted)
	}
	if len(mirror.stored) != 0 {
		t.Errorf("the graph outlived its environment: %+v", mirror.stored)
	}
}

// The mirror exists for other applications to read. Refusing to store a
// simulation because a reader is unreachable would be the wrong trade, so a
// failure is a warning and nothing else.
func TestAFailingMirrorDoesNotFailTheRequest(t *testing.T) {
	mirror := newFakeGraphMirror()
	mirror.setErr, mirror.setCode = errors.New("device-repository is down"), http.StatusInternalServerError
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)

	resp := do(t, router, "POST", "/environments", "user-a", minimalEnvironment())
	if resp.Code != http.StatusCreated {
		t.Fatalf("a failing mirror must not fail the save, got %d: %s", resp.Code, resp.Body.String())
	}
	created := domain.Environment{}
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ExternalGraphRef != "" {
		t.Errorf("no graph was created, so there is no ref to store, got %q", created.ExternalGraphRef)
	}
	if _, stored := store.stored[created.Id]; !stored {
		t.Fatal("the environment itself has to be stored")
	}

	//and the next save picks the mirroring up again, under a fresh graph
	mirror.setErr = nil
	answer := putEnvironment(t, router, created.Id, "user-a", created)
	if answer.ExternalGraphRef == "" {
		t.Error("expected the retry to mirror the environment")
	}
}

// Same for the delete: a graph that stays behind is recoverable by hand, a
// delete that fails over an unreachable reader is not something a caller can
// act on.
func TestAFailingGraphDeleteDoesNotFailTheRequest(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)

	putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())
	mirror.deleteErr, mirror.deleteCode = errors.New("device-repository is down"), http.StatusInternalServerError

	if resp := do(t, router, "DELETE", "/environments/env-1", "user-a", nil); resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", resp.Code, resp.Body.String())
	}
	if _, stillThere := store.stored["env-1"]; stillThere {
		t.Error("the environment has to be gone even though its graph could not be deleted")
	}
}

// A graph somebody already removed by hand is the state the delete wanted. The
// repository answers that with a success of its own, but a 404 from anywhere
// else in the chain means the same thing here.
func TestAGraphThatIsAlreadyGoneIsNotAFailedDelete(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)

	created := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())
	mirror.deleteErr, mirror.deleteCode = errors.New("unexpected statuscode 404: not found"), http.StatusNotFound

	if resp := do(t, router, "DELETE", "/environments/env-1", "user-a", nil); resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(mirror.deleted) != 1 || mirror.deleted[0] != created.ExternalGraphRef {
		t.Errorf("expected the delete to have been attempted once, got %v", mirror.deleted)
	}
}

// The structure alone is what the graph is for: an environment with no devices
// yet is still a site with buildings and rooms, and a consumer that only sees it
// once it has devices sees it appear out of nowhere.
func TestAnEnvironmentWithoutDevicesIsMirroredAnyway(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)

	created := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())
	if created.ExternalGraphRef == "" {
		t.Fatal("an environment without devices was not mirrored")
	}
	graph := mirror.stored[created.ExternalGraphRef]
	for _, node := range graph.Nodes {
		if node.ResourceType == models.GraphResourceTypeDevice {
			t.Fatalf("there is no device here, but a device node exists: %+v", node)
		}
	}
	//the root and the one hall
	if len(graph.Nodes) != 2 {
		t.Errorf("expected the structure to be mirrored, got %+v", graph.Nodes)
	}
}

// The graph is written after provisioning, so a device created by this very save
// is already in the mirror rather than only in the next one.
func TestTheGraphCarriesTheDevicesThisSaveCreated(t *testing.T) {
	mirror := newFakeGraphMirror()
	catalog := &fakeCatalog{idsByName: namedDeviceIds()}
	store := newFakeEnvironments()
	router := testRouterWith(store, catalog, mirror, nil)

	created := putEnvironment(t, router, "env-1", "user-a", environmentWithTwoAssets())

	graph := mirror.stored[created.ExternalGraphRef]
	found := map[string]bool{}
	for _, node := range graph.Nodes {
		if node.ResourceType == models.GraphResourceTypeDevice {
			found[node.ResourceId] = true
		}
	}
	if !found["urn:device:kompressor"] || !found["urn:device:zaehler"] {
		t.Errorf("expected both freshly provisioned devices in the mirror, got %+v", graph.Nodes)
	}
}

// The mirror is rebuilt from the document every time, so it is a mirror and not
// a second document: what somebody changed in a graph editor is gone on the next
// save of the environment.
func TestASavedEnvironmentOverwritesChangesMadeToItsGraph(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)

	created := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())

	//somebody edits the graph directly
	byHand := mirror.stored[created.ExternalGraphRef]
	byHand.Nodes = append(byHand.Nodes, models.Node{Id: "handmade"})
	mirror.stored[created.ExternalGraphRef] = byHand

	putEnvironment(t, router, "env-1", "user-a", created)

	for _, node := range mirror.stored[created.ExternalGraphRef].Nodes {
		if node.Id == "handmade" {
			t.Fatal("a change made to the graph by hand survived a save of the environment")
		}
	}
}

// reconcileGraphRef is the whole ref rule in one function, so the cases that
// have no route of their own are pinned here.
func TestReconcileGraphRef(t *testing.T) {
	t.Run("a document that is new here starts without a ref", func(t *testing.T) {
		env := domain.Environment{ExternalGraphRef: "urn:infai:ses:graph:foreign"}
		reconcileGraphRef(nil, &env)
		if env.ExternalGraphRef != "" {
			t.Errorf("expected the sent ref to be dropped, got %q", env.ExternalGraphRef)
		}
	})
	t.Run("an update keeps the stored ref", func(t *testing.T) {
		previous := domain.Environment{ExternalGraphRef: "urn:infai:ses:graph:owned"}
		env := domain.Environment{ExternalGraphRef: "urn:infai:ses:graph:foreign"}
		reconcileGraphRef(&previous, &env)
		if env.ExternalGraphRef != "urn:infai:ses:graph:owned" {
			t.Errorf("expected the stored ref, got %q", env.ExternalGraphRef)
		}
	})
	t.Run("an update of a document that was never mirrored stays empty", func(t *testing.T) {
		previous := domain.Environment{}
		env := domain.Environment{ExternalGraphRef: "urn:infai:ses:graph:foreign"}
		reconcileGraphRef(&previous, &env)
		if env.ExternalGraphRef != "" {
			t.Errorf("expected an empty ref, got %q", env.ExternalGraphRef)
		}
	})
}
