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

	// setErrFor and panicFor fail the writes of one kind of graph only (see
	// graphKindOf), deleteErrFor the delete of one graph id, so a test can show
	// that the other graph is handled regardless
	setErrFor    map[string]error
	panicFor     map[string]bool
	deleteErrFor map[string]error

	// answerWithoutId stores the graph but answers without its id
	answerWithoutId bool
}

func newFakeGraphMirror() *fakeGraphMirror {
	return &fakeGraphMirror{stored: map[string]models.Graph{},
		setErrFor: map[string]error{}, panicFor: map[string]bool{}, deleteErrFor: map[string]error{}}
}

// graphKindOf tells the two mirrors apart by the attribute only the meter
// graph carries.
func graphKindOf(graph models.Graph) string {
	for _, attr := range graph.Attributes {
		if attr.Key == graphs.GraphAttribute && attr.Value == graphs.MeterGraph {
			return meterGraphKind
		}
	}
	return locationGraphKind
}

func (this *fakeGraphMirror) SetGraph(token string, graph models.Graph) (models.Graph, error, int) {
	this.tokensSeen = append(this.tokensSeen, token)
	this.sent = append(this.sent, graph)
	if this.panicFor[graphKindOf(graph)] {
		panic("graph client bug")
	}
	if err := this.setErrFor[graphKindOf(graph)]; err != nil {
		return models.Graph{}, err, http.StatusInternalServerError
	}
	if this.setErr != nil {
		return models.Graph{}, this.setErr, this.setCode
	}
	if graph.Id == "" {
		this.created++
		graph.Id = fmt.Sprintf("urn:infai:ses:graph:%d", this.created)
	}
	this.stored[graph.Id] = graph
	if this.answerWithoutId {
		return models.Graph{}, nil, http.StatusOK
	}
	return graph, nil, http.StatusOK
}

func (this *fakeGraphMirror) DeleteGraph(token string, id string) (error, int) {
	this.tokensSeen = append(this.tokensSeen, token)
	this.deleted = append(this.deleted, id)
	if err := this.deleteErrFor[id]; err != nil {
		return err, http.StatusInternalServerError
	}
	if this.deleteErr != nil {
		return this.deleteErr, this.deleteCode
	}
	delete(this.stored, id)
	return nil, http.StatusOK
}

// lastSent is the last graph of one kind that was written.
func (this *fakeGraphMirror) lastSent(t *testing.T, kind string) models.Graph {
	t.Helper()
	for i := len(this.sent) - 1; i >= 0; i-- {
		if graphKindOf(this.sent[i]) == kind {
			return this.sent[i]
		}
	}
	t.Fatalf("no %s graph was mirrored at all", kind)
	return models.Graph{}
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
	if created.ExternalMeterGraphRef != "urn:infai:ses:graph:2" {
		t.Fatalf("expected the assigned meter graph ref in the answer, got %q", created.ExternalMeterGraphRef)
	}
	if stored := store.stored[created.Id].ExternalGraphRef; stored != created.ExternalGraphRef {
		t.Errorf("the ref has to be in the stored document, not only in the answer, got %q", stored)
	}
	if stored := store.stored[created.Id].ExternalMeterGraphRef; stored != created.ExternalMeterGraphRef {
		t.Errorf("the meter graph ref has to be in the stored document, not only in the answer, got %q", stored)
	}
	for _, kind := range []string{locationGraphKind, meterGraphKind} {
		if id := mirror.lastSent(t, kind).Id; id != "" {
			t.Errorf("a %s graph that does not exist yet is created without an id, got %q", kind, id)
		}
	}
	if graphName(mirror.stored["urn:infai:ses:graph:1"]) != "Metallbau Musterstadt" {
		t.Errorf("expected the environment name on the mirrored graph, got %+v", mirror.stored)
	}
	if graphName(mirror.stored["urn:infai:ses:graph:2"]) != "Metallbau Musterstadt (meters)" {
		t.Errorf("expected the meter graph under the second ref, got %+v", mirror.stored)
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
	stored.ExternalMeterGraphRef = "urn:infai:ses:graph:owned-meters"
	store.stored["env-1"] = stored

	//the client echoes different refs, whether from a stale copy or by hand
	sending := copyEnvironment(t, stored)
	sending.ExternalGraphRef = "urn:infai:ses:graph:somebody-else"
	sending.ExternalMeterGraphRef = "urn:infai:ses:graph:somebody-elses-meters"
	sending.Name = "Metallbau Musterstadt, neu"
	answer := putEnvironment(t, router, "env-1", "user-a", sending)

	if answer.ExternalGraphRef != "urn:infai:ses:graph:owned" {
		t.Errorf("expected the stored ref to survive, got %q", answer.ExternalGraphRef)
	}
	if answer.ExternalMeterGraphRef != "urn:infai:ses:graph:owned-meters" {
		t.Errorf("expected the stored meter graph ref to survive, got %q", answer.ExternalMeterGraphRef)
	}
	if id := mirror.lastSent(t, locationGraphKind).Id; id != "urn:infai:ses:graph:owned" {
		t.Fatalf("expected the update to address the stored graph, it addressed %q", id)
	}
	if id := mirror.lastSent(t, meterGraphKind).Id; id != "urn:infai:ses:graph:owned-meters" {
		t.Fatalf("expected the update to address the stored meter graph, it addressed %q", id)
	}
	for _, foreign := range []string{"urn:infai:ses:graph:somebody-else", "urn:infai:ses:graph:somebody-elses-meters"} {
		if _, touched := mirror.stored[foreign]; touched {
			t.Errorf("the graph %s named by the client must not be written", foreign)
		}
	}
	if graphName(mirror.stored["urn:infai:ses:graph:owned"]) != "Metallbau Musterstadt, neu" {
		t.Error("the mirror was not brought up to date with the new document")
	}
	if graphName(mirror.stored["urn:infai:ses:graph:owned-meters"]) != "Metallbau Musterstadt, neu (meters)" {
		t.Error("the meter graph was not brought up to date with the new document")
	}
	if len(mirror.stored) != 2 {
		t.Errorf("an update must not create graphs, got %d", len(mirror.stored))
	}
}

// The dangerous one: an export put under a new id is a copy, and its ref still
// points at the graph of the original. A copy owns nothing.
func TestACopyUnderANewIdGetsItsOwnGraphAndLeavesTheOriginalAlone(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)

	original := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())
	if original.ExternalGraphRef == "" || original.ExternalMeterGraphRef == "" {
		t.Fatal("setup: the original was not mirrored")
	}
	originalGraph := mirror.stored[original.ExternalGraphRef]
	originalMeterGraph := mirror.stored[original.ExternalMeterGraphRef]

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
	if answer.ExternalMeterGraphRef == "" || answer.ExternalMeterGraphRef == original.ExternalMeterGraphRef ||
		answer.ExternalMeterGraphRef == answer.ExternalGraphRef {
		t.Fatalf("the copy needs a meter graph of its own, got %q next to the original's %q",
			answer.ExternalMeterGraphRef, original.ExternalMeterGraphRef)
	}
	if got := graphName(mirror.stored[original.ExternalMeterGraphRef]); got != graphName(originalMeterGraph) {
		t.Errorf("the meter graph of the original was overwritten, its name is now %q", got)
	}
	if got := graphName(mirror.stored[answer.ExternalMeterGraphRef]); got != "Metallbau Zweitwerk (meters)" {
		t.Errorf("expected the copy's own meter graph to carry its name, got %q", got)
	}

	//and deleting the copy leaves the original's graphs standing
	if resp := do(t, router, "DELETE", "/environments/env-2", "user-a", nil); resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", resp.Code, resp.Body.String())
	}
	if _, alive := mirror.stored[original.ExternalGraphRef]; !alive {
		t.Error("deleting the copy deleted the graph of the original")
	}
	if _, alive := mirror.stored[original.ExternalMeterGraphRef]; !alive {
		t.Error("deleting the copy deleted the meter graph of the original")
	}
}

// Deleting an environment deletes both mirrors: a graph nobody can reach from an
// environment is a location that no longer exists.
func TestDeletingAnEnvironmentDeletesItsGraph(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)

	created := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())

	if resp := do(t, router, "DELETE", "/environments/env-1", "user-a", nil); resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(mirror.deleted) != 2 || mirror.deleted[0] != created.ExternalGraphRef || mirror.deleted[1] != created.ExternalMeterGraphRef {
		t.Fatalf("expected exactly the two graphs of this environment to be deleted, got %v", mirror.deleted)
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
	if created.ExternalGraphRef != "" || created.ExternalMeterGraphRef != "" {
		t.Errorf("no graph was created, so there is no ref to store, got %q and %q", created.ExternalGraphRef, created.ExternalMeterGraphRef)
	}
	if _, stored := store.stored[created.Id]; !stored {
		t.Fatal("the environment itself has to be stored")
	}

	//and the next save picks the mirroring up again, under a fresh graph
	mirror.setErr = nil
	answer := putEnvironment(t, router, created.Id, "user-a", created)
	if answer.ExternalGraphRef == "" || answer.ExternalMeterGraphRef == "" {
		t.Error("expected the retry to mirror the environment")
	}
}

// The two mirrors are written independently: a failure of one is a warning of
// its own and leaves the other written, its ref stored and the save successful.
func TestAFailingGraphLeavesTheOtherGraphWritten(t *testing.T) {
	for _, failing := range []string{locationGraphKind, meterGraphKind} {
		for _, how := range []string{"error", "panic"} {
			t.Run(failing+" "+how, func(t *testing.T) {
				mirror := newFakeGraphMirror()
				if how == "panic" {
					mirror.panicFor[failing] = true
				} else {
					mirror.setErrFor[failing] = errors.New("device-repository refused it")
				}
				store := newFakeEnvironments()
				router := testRouterWith(store, nil, mirror, nil)

				resp := do(t, router, "POST", "/environments", "user-a", minimalEnvironment())
				if resp.Code != http.StatusCreated {
					t.Fatalf("a failing %s graph must not fail the save, got %d: %s", failing, resp.Code, resp.Body.String())
				}
				created := domain.Environment{}
				if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
					t.Fatal(err)
				}
				stored := store.stored[created.Id]
				failed, written := stored.ExternalGraphRef, stored.ExternalMeterGraphRef
				if failing == meterGraphKind {
					failed, written = written, failed
				}
				if failed != "" {
					t.Errorf("the failing graph was never created, so it has no ref, got %q", failed)
				}
				if written == "" || len(mirror.stored) != 1 {
					t.Fatalf("the other graph has to be written and its ref stored, ref %q, graphs %d", written, len(mirror.stored))
				}
				if _, ok := mirror.stored[written]; !ok {
					t.Errorf("the stored ref %q names no written graph", written)
				}
			})
		}
	}
}

// An answer without an id must not blank a ref that worked: the graph would be
// orphaned and a second one created on the next save.
func TestAnAnswerWithoutAnIdKeepsBothRefs(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)
	created := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())

	mirror.answerWithoutId = true
	answer := putEnvironment(t, router, "env-1", "user-a", created)

	if answer.ExternalGraphRef != created.ExternalGraphRef || answer.ExternalMeterGraphRef != created.ExternalMeterGraphRef {
		t.Errorf("expected both refs kept, got %q and %q", answer.ExternalGraphRef, answer.ExternalMeterGraphRef)
	}
}

// An update whose write of one graph fails keeps that graph's stored ref, so the
// next save rewrites the same graph instead of creating a second one.
func TestAFailingGraphUpdateKeepsItsRef(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)
	created := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())

	mirror.setErrFor[meterGraphKind] = errors.New("device-repository refused it")
	created.Name = "Metallbau Musterstadt, neu"
	answer := putEnvironment(t, router, "env-1", "user-a", created)

	if answer.ExternalMeterGraphRef != created.ExternalMeterGraphRef || store.stored["env-1"].ExternalMeterGraphRef != created.ExternalMeterGraphRef {
		t.Errorf("a failed rewrite must keep the meter graph ref, got %q", store.stored["env-1"].ExternalMeterGraphRef)
	}
	if graphName(mirror.stored[created.ExternalGraphRef]) != "Metallbau Musterstadt, neu" {
		t.Error("the location graph has to be rewritten regardless")
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

// A graph that cannot be deleted does not keep the other one alive.
func TestAFailingDeleteOfOneGraphStillDeletesTheOther(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)

	created := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())
	mirror.deleteErrFor[created.ExternalGraphRef] = errors.New("device-repository is down")

	if resp := do(t, router, "DELETE", "/environments/env-1", "user-a", nil); resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", resp.Code, resp.Body.String())
	}
	if _, alive := mirror.stored[created.ExternalMeterGraphRef]; alive {
		t.Error("the meter graph has to be deleted although the location graph could not be")
	}
	if _, alive := mirror.stored[created.ExternalGraphRef]; !alive {
		t.Error("setup: the location graph delete was supposed to fail")
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
	if len(mirror.deleted) != 2 || mirror.deleted[0] != created.ExternalGraphRef || mirror.deleted[1] != created.ExternalMeterGraphRef {
		t.Errorf("expected the delete to have been attempted once per graph, got %v", mirror.deleted)
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
	foreign := func() domain.Environment {
		return domain.Environment{ExternalGraphRef: "urn:infai:ses:graph:foreign", ExternalMeterGraphRef: "urn:infai:ses:graph:foreign-meters"}
	}
	t.Run("a document that is new here starts without a ref", func(t *testing.T) {
		env := foreign()
		reconcileGraphRef(nil, &env)
		if env.ExternalGraphRef != "" || env.ExternalMeterGraphRef != "" {
			t.Errorf("expected the sent refs to be dropped, got %q and %q", env.ExternalGraphRef, env.ExternalMeterGraphRef)
		}
	})
	t.Run("an update keeps the stored ref", func(t *testing.T) {
		previous := domain.Environment{ExternalGraphRef: "urn:infai:ses:graph:owned", ExternalMeterGraphRef: "urn:infai:ses:graph:owned-meters"}
		env := foreign()
		reconcileGraphRef(&previous, &env)
		if env.ExternalGraphRef != "urn:infai:ses:graph:owned" || env.ExternalMeterGraphRef != "urn:infai:ses:graph:owned-meters" {
			t.Errorf("expected the stored refs, got %q and %q", env.ExternalGraphRef, env.ExternalMeterGraphRef)
		}
	})
	t.Run("an update of a document that was never mirrored stays empty", func(t *testing.T) {
		previous := domain.Environment{}
		env := foreign()
		reconcileGraphRef(&previous, &env)
		if env.ExternalGraphRef != "" || env.ExternalMeterGraphRef != "" {
			t.Errorf("expected empty refs, got %q and %q", env.ExternalGraphRef, env.ExternalMeterGraphRef)
		}
	})
	t.Run("a document stored before the meter graph existed gets one", func(t *testing.T) {
		previous := domain.Environment{ExternalGraphRef: "urn:infai:ses:graph:owned"}
		env := foreign()
		reconcileGraphRef(&previous, &env)
		if env.ExternalGraphRef != "urn:infai:ses:graph:owned" || env.ExternalMeterGraphRef != "" {
			t.Errorf("expected the stored ref and no meter graph ref, got %q and %q", env.ExternalGraphRef, env.ExternalMeterGraphRef)
		}
	})
}

// The meter graph is built from the document as it will be stored: a device
// this save created is already in it, under the parents meter_parents names.
func TestTheMeterGraphCarriesTheMeterParentsOfTheSave(t *testing.T) {
	mirror := newFakeGraphMirror()
	catalog := &fakeCatalog{idsByName: namedDeviceIds()}
	store := newFakeEnvironments()
	router := testRouterWith(store, catalog, mirror, nil)

	env := environmentWithTwoAssets()
	env.Zones[0].Assets[0].Id = "asset-kompressor"
	env.Zones[0].Zones[0].Assets[0].Id = "asset-zaehler"
	env.MeterGroups = []domain.MeterGroup{{Id: "group-abgaenge", Name: "Abgänge", Parents: []domain.MeterParent{{Id: "asset-zaehler"}}}}
	env.Zones[0].Assets[0].MeterParents = []domain.MeterParent{{Id: "group-abgaenge"}}
	created := putEnvironment(t, router, "env-1", "user-a", env)

	graph := mirror.stored[created.ExternalMeterGraphRef]
	want := map[string]string{
		"urn:device:kompressor": "group-abgaenge",
		"group-abgaenge":        "urn:device:zaehler",
		"urn:device:zaehler":    graphs.RootNodeId,
	}
	if len(graph.Edges) != len(want) {
		t.Fatalf("expected %d edges, got %+v", len(want), graph.Edges)
	}
	for _, edge := range graph.Edges {
		if want[edge.FromNodeId] != edge.ToNodeId || edge.Weight != 100 {
			t.Errorf("unexpected edge %+v", edge)
		}
	}
	if err := graph.Valid(); err != nil {
		t.Errorf("the repository would refuse the meter graph: %v", err)
	}
	//the location graph knows nothing of it
	for _, node := range mirror.stored[created.ExternalGraphRef].Nodes {
		if node.Id == "group-abgaenge" {
			t.Error("a meter group must not appear in the location graph")
		}
	}
}
