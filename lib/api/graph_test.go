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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/graphs"
	"github.com/SENERGY-Platform/moses/lib/util"
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
	setPanics  bool
	deleteErr  error
	deleteCode int

	// deleteErrFor fails the delete of one graph id only, so a test can show
	// that another graph is handled regardless
	deleteErrFor map[string]error

	// answerWithoutId stores the graph but answers without its id
	answerWithoutId bool

	// gone holds graphs somebody deleted: a write to one is refused with 403,
	// as the repository refuses a graph whose rights went with it
	gone map[string]bool
	// createErr fails a create only, with 500
	createErr error
	// onCreate runs for every graph created, as the repository gives its owner
	// initial rights
	onCreate func(id string, owner string)
}

func newFakeGraphMirror() *fakeGraphMirror {
	return &fakeGraphMirror{stored: map[string]models.Graph{}, deleteErrFor: map[string]error{}, gone: map[string]bool{}}
}

func (this *fakeGraphMirror) SetGraph(token string, graph models.Graph) (models.Graph, error, int) {
	this.tokensSeen = append(this.tokensSeen, token)
	this.sent = append(this.sent, graph)
	if this.setPanics {
		panic("graph client bug")
	}
	if this.setErr != nil {
		return models.Graph{}, this.setErr, this.setCode
	}
	if this.gone[graph.Id] {
		return models.Graph{}, errors.New("access denied"), http.StatusForbidden
	}
	if graph.Id == "" {
		if this.createErr != nil {
			return models.Graph{}, this.createErr, http.StatusInternalServerError
		}
		this.created++
		graph.Id = fmt.Sprintf("urn:infai:ses:graph:%d", this.created)
		if this.onCreate != nil {
			this.onCreate(graph.Id, graph.Owner)
		}
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

// lastSent is the last graph that was written.
func (this *fakeGraphMirror) lastSent(t *testing.T) models.Graph {
	t.Helper()
	if len(this.sent) == 0 {
		t.Fatal("no graph was mirrored at all")
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

// captureLogs collects what the package logger writes during one call. Safe
// only around handlers that log from the calling goroutine, since util.Logger is
// a package variable.
func captureLogs(t *testing.T, during func()) string {
	t.Helper()
	buffer := &bytes.Buffer{}
	previous := util.Logger
	util.Logger = slog.New(slog.NewTextHandler(buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	defer func() { util.Logger = previous }()
	during()
	return buffer.String()
}

// withBodyField is the JSON of env with one more top level field, for a field
// the domain type does not decode.
func withBodyField(t *testing.T, env domain.Environment, key string, value interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]interface{}{}
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	result[key] = value
	return result
}

// environmentWithMeterParents carries meter_parents and a meter group, which
// moses validates and stores but no longer mirrors.
func environmentWithMeterParents() domain.Environment {
	env := environmentWithTwoAssets()
	env.Zones[0].Assets[0].Id = "asset-kompressor"
	env.Zones[0].Zones[0].Assets[0].Id = "asset-zaehler"
	env.MeterGroups = []domain.MeterGroup{{Id: "group-abgaenge", Name: "Abgänge", Parents: []domain.MeterParent{{Id: "asset-zaehler"}}}}
	env.Zones[0].Assets[0].MeterParents = []domain.MeterParent{{Id: "group-abgaenge"}}
	return env
}

const (
	ownedGraph      = "urn:infai:ses:graph:owned"
	leftoverMeters  = "urn:infai:ses:graph:owned-meters"
	somebodysMeters = "urn:infai:ses:graph:somebody-elses-meters"
)

// storeWithLeftoverMeterGraph holds an environment saved while moses still
// wrote a meter graph, and the mirror holds both of its graphs.
func storeWithLeftoverMeterGraph(t *testing.T, store *fakeEnvironments, mirror *fakeGraphMirror) domain.Environment {
	t.Helper()
	stored := storeDirectly(t, store, "env-1", "user-a", minimalEnvironment())
	stored.ExternalGraphRef = ownedGraph
	stored.ExternalMeterGraphRef = leftoverMeters
	store.stored["env-1"] = stored
	mirror.stored[ownedGraph] = models.Graph{Id: ownedGraph}
	mirror.stored[leftoverMeters] = models.Graph{Id: leftoverMeters}
	return stored
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

// meter_parents and meter_groups are stored, but a create writes the location
// graph only and stores no meter graph ref. Through POST and through a PUT to
// a new id alike.
func TestACreateWithMeterParentsWritesExactlyOneGraph(t *testing.T) {
	for _, how := range []string{"POST", "PUT"} {
		t.Run(how, func(t *testing.T) {
			mirror := newFakeGraphMirror()
			catalog := &fakeCatalog{idsByName: namedDeviceIds()}
			store := newFakeEnvironments()
			router := testRouterWith(store, catalog, mirror, nil)

			path := "/environments"
			if how == "PUT" {
				path += "/env-1"
			}
			resp := do(t, router, how, path, "user-a", environmentWithMeterParents())
			if resp.Code != http.StatusCreated && resp.Code != http.StatusOK {
				t.Fatalf("expected the create to succeed, got %d: %s", resp.Code, resp.Body.String())
			}
			if len(mirror.sent) != 1 || len(mirror.stored) != 1 {
				t.Fatalf("expected exactly one graph written, got %d writes and %d graphs", len(mirror.sent), len(mirror.stored))
			}
			if len(store.stored) != 1 {
				t.Fatalf("expected one stored environment, got %d", len(store.stored))
			}
			for _, stored := range store.stored {
				if stored.ExternalGraphRef == "" || stored.ExternalMeterGraphRef != "" {
					t.Errorf("expected only the location graph ref stored, got %q and %q", stored.ExternalGraphRef, stored.ExternalMeterGraphRef)
				}
				if len(stored.MeterGroups) != 1 || len(assetNamed(t, stored, "Kompressor 1").MeterParents) != 1 {
					t.Errorf("meter_parents and meter_groups have to be stored as sent, got %+v", stored.MeterGroups)
				}
			}
			for _, node := range mirror.lastSent(t).Nodes {
				if node.Id == "group-abgaenge" {
					t.Error("a meter group must not appear in the location graph")
				}
			}
			if strings.Contains(resp.Body.String(), "external_meter_graph_ref") {
				t.Errorf("the answer must not carry a meter graph ref, got %s", resp.Body.String())
			}
		})
	}
}

// The whole document is sent on every update, so the ref it carries is worth
// nothing: what is stored decides which graph is written.
func TestUpdatingAnEnvironmentWritesTheStoredGraphNotTheSentOne(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)

	stored := storeDirectly(t, store, "env-1", "user-a", minimalEnvironment())
	stored.ExternalGraphRef = ownedGraph
	store.stored["env-1"] = stored

	//the client echoes a different ref, whether from a stale copy or by hand
	sending := copyEnvironment(t, stored)
	sending.ExternalGraphRef = "urn:infai:ses:graph:somebody-else"
	sending.Name = "Metallbau Musterstadt, neu"
	answer := putEnvironment(t, router, "env-1", "user-a", sending)

	if answer.ExternalGraphRef != ownedGraph {
		t.Errorf("expected the stored ref to survive, got %q", answer.ExternalGraphRef)
	}
	if id := mirror.lastSent(t).Id; id != ownedGraph {
		t.Fatalf("expected the update to address the stored graph, it addressed %q", id)
	}
	if _, touched := mirror.stored["urn:infai:ses:graph:somebody-else"]; touched {
		t.Error("the graph named by the client must not be written")
	}
	if graphName(mirror.stored[ownedGraph]) != "Metallbau Musterstadt, neu" {
		t.Error("the mirror was not brought up to date with the new document")
	}
	if len(mirror.stored) != 1 {
		t.Errorf("an update must not create graphs, got %d", len(mirror.stored))
	}
}

// A meter graph ref in the body is never taken: a save deletes the meter graph
// the stored document names, so honouring a sent one would delete somebody
// else's graph.
func TestAMeterGraphRefInTheBodyIsIgnored(t *testing.T) {
	cases := map[string]func(t *testing.T, store *fakeEnvironments) (method string, path string){
		"create": func(t *testing.T, store *fakeEnvironments) (string, string) {
			return "POST", "/environments"
		},
		"put to a new id": func(t *testing.T, store *fakeEnvironments) (string, string) {
			return "PUT", "/environments/env-2"
		},
		"update of a document without one": func(t *testing.T, store *fakeEnvironments) (string, string) {
			stored := storeDirectly(t, store, "env-1", "user-a", minimalEnvironment())
			stored.ExternalGraphRef = ownedGraph
			store.stored["env-1"] = stored
			return "PUT", "/environments/env-1"
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			mirror := newFakeGraphMirror()
			mirror.stored[somebodysMeters] = models.Graph{Id: somebodysMeters}
			store := newFakeEnvironments()
			router := testRouterWith(store, nil, mirror, nil)
			method, path := setup(t, store)

			resp := do(t, router, method, path, "user-a",
				withBodyField(t, minimalEnvironment(), "external_meter_graph_ref", somebodysMeters))
			if resp.Code != http.StatusCreated && resp.Code != http.StatusOK {
				t.Fatalf("expected the save to succeed, got %d: %s", resp.Code, resp.Body.String())
			}
			if len(mirror.deleted) != 0 {
				t.Errorf("a ref from the body must not be deleted, deleted %v", mirror.deleted)
			}
			if _, alive := mirror.stored[somebodysMeters]; !alive {
				t.Error("the graph named in the body is gone")
			}
			for id, stored := range store.stored {
				if stored.ExternalMeterGraphRef != "" {
					t.Errorf("%s: a ref from the body must not be stored, got %q", id, stored.ExternalMeterGraphRef)
				}
			}
		})
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

// A save of a document that still names a meter graph deletes that graph and
// stores the document without the ref. The location graph is rewritten as
// always, and the meter graph is never written again.
func TestASaveDeletesALeftoverMeterGraph(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)
	stored := storeWithLeftoverMeterGraph(t, store, mirror)

	sending := copyEnvironment(t, stored)
	sending.Name = "Metallbau Musterstadt, neu"
	putEnvironment(t, router, "env-1", "user-a", sending)

	if len(mirror.deleted) != 1 || mirror.deleted[0] != leftoverMeters {
		t.Fatalf("expected exactly the meter graph to be deleted, got %v", mirror.deleted)
	}
	if _, alive := mirror.stored[leftoverMeters]; alive {
		t.Error("the meter graph outlived the save")
	}
	if ref := store.stored["env-1"].ExternalMeterGraphRef; ref != "" {
		t.Errorf("the deleted meter graph's ref has to be gone from the stored document, got %q", ref)
	}
	if store.stored["env-1"].ExternalGraphRef != ownedGraph || graphName(mirror.stored[ownedGraph]) != "Metallbau Musterstadt, neu" {
		t.Error("the location graph has to be rewritten under its stored ref")
	}
	for _, graph := range mirror.sent {
		if graph.Id == leftoverMeters {
			t.Error("the meter graph must not be written again")
		}
	}
}

// A meter graph that cannot be deleted keeps its ref, so the next save tries
// again; the request itself succeeds and the failure is a warning naming the
// environment and the graph.
func TestAFailedDeleteOfALeftoverMeterGraphKeepsItsRef(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)
	stored := storeWithLeftoverMeterGraph(t, store, mirror)
	mirror.deleteErrFor[leftoverMeters] = errors.New("device-repository is down")

	sending := copyEnvironment(t, stored)
	var resp *httptest.ResponseRecorder
	logs := captureLogs(t, func() {
		resp = do(t, router, "PUT", "/environments/env-1", "user-a", sending)
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("a failed delete must not fail the save, got %d: %s", resp.Code, resp.Body.String())
	}
	if ref := store.stored["env-1"].ExternalMeterGraphRef; ref != leftoverMeters {
		t.Fatalf("a failed delete has to keep the ref, got %q", ref)
	}
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "environment=env-1") || !strings.Contains(logs, "graph="+leftoverMeters) {
		t.Errorf("expected a warning naming the environment and the graph, got %s", logs)
	}

	//and the next save that can delete it does
	delete(mirror.deleteErrFor, leftoverMeters)
	putEnvironment(t, router, "env-1", "user-a", copyEnvironment(t, stored))
	if ref := store.stored["env-1"].ExternalMeterGraphRef; ref != "" {
		t.Errorf("the retry has to clear the ref, got %q", ref)
	}
	if _, alive := mirror.stored[leftoverMeters]; alive {
		t.Error("the retry has to delete the meter graph")
	}
}

// A meter graph that is already gone is what the save wanted, so a 404 clears
// the ref like a delete that worked.
func TestALeftoverMeterGraphThatIsAlreadyGoneClearsTheRef(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)
	stored := storeWithLeftoverMeterGraph(t, store, mirror)
	mirror.deleteErr, mirror.deleteCode = errors.New("unexpected statuscode 404: not found"), http.StatusNotFound

	putEnvironment(t, router, "env-1", "user-a", copyEnvironment(t, stored))

	if ref := store.stored["env-1"].ExternalMeterGraphRef; ref != "" {
		t.Errorf("a meter graph that is already gone leaves no ref, got %q", ref)
	}
}

// A save refused for a stale version has no side effects, and the delete of a
// leftover meter graph is one.
func TestARefusedSaveLeavesTheLeftoverMeterGraphAlone(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)
	stored := storeWithLeftoverMeterGraph(t, store, mirror)
	stored.Version = 3
	store.stored["env-1"] = stored

	sending := copyEnvironment(t, stored)
	sending.Version = 2
	if resp := do(t, router, "PUT", "/environments/env-1", "user-a", sending); resp.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(mirror.deleted) != 0 {
		t.Errorf("a refused save must not delete a graph, deleted %v", mirror.deleted)
	}
	if ref := store.stored["env-1"].ExternalMeterGraphRef; ref != leftoverMeters {
		t.Errorf("a refused save must not touch the stored ref, got %q", ref)
	}
}

// Without a device-repository behind it moses cannot delete the meter graph,
// so the ref stays for a deployment that has one.
func TestWithoutAMirrorALeftoverMeterGraphRefIsKept(t *testing.T) {
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, nil, nil)
	stored := storeWithLeftoverMeterGraph(t, store, newFakeGraphMirror())

	putEnvironment(t, router, "env-1", "user-a", copyEnvironment(t, stored))

	if ref := store.stored["env-1"].ExternalMeterGraphRef; ref != leftoverMeters {
		t.Errorf("expected the ref kept, got %q", ref)
	}
}

// The export is what PUT accepts, and a meter graph ref is not part of it.
func TestTheExportCarriesNoMeterGraphRef(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)
	storeWithLeftoverMeterGraph(t, store, mirror)

	resp := do(t, router, "GET", "/environments/env-1", "user-a", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "external_meter_graph_ref") || strings.Contains(resp.Body.String(), leftoverMeters) {
		t.Errorf("the export must not carry the meter graph ref, got %s", resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `"external_graph_ref":"`+ownedGraph+`"`) {
		t.Errorf("the export still carries the location graph ref, got %s", resp.Body.String())
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

// A meter graph a document still names goes with the environment as well.
func TestDeletingAnEnvironmentDeletesALeftoverMeterGraph(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)
	storeWithLeftoverMeterGraph(t, store, mirror)

	if resp := do(t, router, "DELETE", "/environments/env-1", "user-a", nil); resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(mirror.deleted) != 2 || mirror.deleted[0] != ownedGraph || mirror.deleted[1] != leftoverMeters {
		t.Fatalf("expected the location graph and the meter graph to be deleted, got %v", mirror.deleted)
	}
	if len(mirror.stored) != 0 {
		t.Errorf("a graph outlived its environment: %+v", mirror.stored)
	}
}

// The mirror exists for other applications to read. Refusing to store a
// simulation because a reader is unreachable would be the wrong trade, so a
// failure is a warning and nothing else.
func TestAFailingMirrorDoesNotFailTheRequest(t *testing.T) {
	for _, how := range []string{"error", "panic"} {
		t.Run(how, func(t *testing.T) {
			mirror := newFakeGraphMirror()
			if how == "panic" {
				mirror.setPanics = true
			} else {
				mirror.setErr, mirror.setCode = errors.New("device-repository is down"), http.StatusInternalServerError
			}
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
			mirror.setErr, mirror.setPanics = nil, false
			answer := putEnvironment(t, router, created.Id, "user-a", created)
			if answer.ExternalGraphRef == "" {
				t.Error("expected the retry to mirror the environment")
			}
		})
	}
}

// An answer without an id must not blank a ref that worked: the graph would be
// orphaned and a second one created on the next save.
func TestAnAnswerWithoutAnIdKeepsTheRef(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)
	created := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())

	mirror.answerWithoutId = true
	answer := putEnvironment(t, router, "env-1", "user-a", created)

	if answer.ExternalGraphRef != created.ExternalGraphRef {
		t.Errorf("expected the ref kept, got %q", answer.ExternalGraphRef)
	}
}

// An update whose graph write fails keeps the stored ref, so the next save
// rewrites the same graph instead of creating a second one.
func TestAFailingGraphUpdateKeepsItsRef(t *testing.T) {
	mirror := newFakeGraphMirror()
	store := newFakeEnvironments()
	router := testRouterWith(store, nil, mirror, nil)
	created := putEnvironment(t, router, "env-1", "user-a", minimalEnvironment())

	mirror.setErr, mirror.setCode = errors.New("device-repository refused it"), http.StatusInternalServerError
	created.Name = "Metallbau Musterstadt, neu"
	answer := putEnvironment(t, router, "env-1", "user-a", created)

	if answer.ExternalGraphRef != created.ExternalGraphRef || store.stored["env-1"].ExternalGraphRef != created.ExternalGraphRef {
		t.Errorf("a failed rewrite must keep the graph ref, got %q", store.stored["env-1"].ExternalGraphRef)
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
	storeWithLeftoverMeterGraph(t, store, mirror)
	mirror.deleteErrFor[ownedGraph] = errors.New("device-repository is down")

	if resp := do(t, router, "DELETE", "/environments/env-1", "user-a", nil); resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", resp.Code, resp.Body.String())
	}
	if _, alive := mirror.stored[leftoverMeters]; alive {
		t.Error("the meter graph has to be deleted although the location graph could not be")
	}
	if _, alive := mirror.stored[ownedGraph]; !alive {
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
	t.Run("an update keeps the stored refs", func(t *testing.T) {
		previous := domain.Environment{ExternalGraphRef: ownedGraph, ExternalMeterGraphRef: leftoverMeters}
		env := foreign()
		reconcileGraphRef(&previous, &env)
		if env.ExternalGraphRef != ownedGraph || env.ExternalMeterGraphRef != leftoverMeters {
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
}
