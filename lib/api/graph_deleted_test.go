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
	"net/http"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// ---------------------------------------------------------------------------
// A graph somebody deleted is created anew by the next save, but only when
// permissions-v2 says it stores no rights for it: a 403 alone could be
// transient, and creating on it would leave two graphs.
// ---------------------------------------------------------------------------

// deletedGraphSetup holds env-1 with testGraph as its stored ref and the set of
// TestAGraphCreatedByASaveInheritsTheGraphAdmins. The mirror numbers new graphs
// from 11, so a new one cannot be mistaken for testGraph.
func deletedGraphSetup() (*fakeEnvironments, *fakeShares, *fakePermissions, *fakeGraphMirror) {
	shares := newFakeShares()
	shares.setWithGraphAdmins("env-1", []string{"demo-user", "writer", "reader"}, []string{"/demo"},
		repo.Principals{Users: []string{"writer"}}, repo.Principals{Users: []string{"demo-user"}, Groups: []string{"/demo"}})
	permissions := newFakePermissions("dev-1", "dev-2")
	mirror := newFakeGraphMirror()
	mirror.created = 10
	mirror.onCreate = func(id string, owner string) { permissions.ownGraph(id, owner) }
	return storeWithGraphEnvironment(), shares, permissions, mirror
}

func saveEnvironmentWithGraph(t *testing.T, store *fakeEnvironments, shares *fakeShares, permissions Permissions, mirror *fakeGraphMirror) (domain.Environment, string) {
	t.Helper()
	router := testRouterWithAll(store, shares, nil, mirror, nil, permissions)
	answer := domain.Environment{}
	logs := captureLogs(t, func() {
		resp := do(t, router, "PUT", "/environments/env-1", "user-a", sharedEnvironmentWithGraph())
		if resp.Code != http.StatusOK {
			t.Fatalf("a save must succeed whatever happens to the graph, got %d: %s", resp.Code, resp.Body.String())
		}
		answer = decodeEnvironment(t, resp.Body.Bytes())
	})
	return answer, logs
}

func decodeEnvironment(t *testing.T, body []byte) domain.Environment {
	t.Helper()
	result := domain.Environment{}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestASaveCreatesANewGraphWhenTheStoredOneWasDeleted(t *testing.T) {
	store, shares, permissions, mirror := deletedGraphSetup()
	//deleted: the repository refuses the write, permissions-v2 knows no rights
	mirror.gone[testGraph] = true

	answer, logs := saveEnvironmentWithGraph(t, store, shares, permissions, mirror)

	stored := store.stored["env-1"].ExternalGraphRef
	if stored == "" || stored == testGraph {
		t.Fatalf("expected a new graph to be stored, got %q", stored)
	}
	if answer.ExternalGraphRef != stored {
		t.Errorf("the answer has to carry the new ref, got %q", answer.ExternalGraphRef)
	}
	if len(mirror.sent) != 2 || mirror.sent[0].Id != testGraph || mirror.sent[1].Id != "" {
		t.Fatalf("expected a write to the old graph, then one create, got %+v", mirror.sent)
	}
	if _, created := mirror.stored[stored]; !created {
		t.Error("the new ref has to name the created graph")
	}
	assertSharedGraph(t, permissions, stored)
	if !strings.Contains(logs, "created a new graph for an environment whose graph was deleted") {
		t.Errorf("expected the replacement in the log, got %s", logs)
	}

	//and the next save writes the new graph, without asking again
	mirror.sent = nil
	gets, _, _ := permissions.calls()
	before := len(gets)
	if resp := do(t, testRouterWithAll(store, shares, nil, mirror, nil, permissions), "PUT", "/environments/env-1", "user-a", answer); resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(mirror.sent) != 1 || mirror.sent[0].Id != stored {
		t.Errorf("expected one write to the new graph, got %+v", mirror.sent)
	}
	if gets, _, _ = permissions.calls(); len(gets) != before {
		t.Errorf("a graph that exists needs no check, got %v", gets[before:])
	}
}

// A 403 for a graph whose rights still exist is about the caller, not about the
// graph: no second graph.
func TestATransientRefusalDoesNotCreateASecondGraph(t *testing.T) {
	store, shares, permissions, mirror := deletedGraphSetup()
	mirror.gone[testGraph] = true
	permissions.ownGraph(testGraph, "user-a")

	answer, logs := saveEnvironmentWithGraph(t, store, shares, permissions, mirror)

	assertGraphKept(t, store, mirror, answer)
	if !strings.Contains(logs, "unable to mirror an environment as a graph") || !strings.Contains(logs, "level=WARN") {
		t.Errorf("expected the refused write as a warning, got %s", logs)
	}
}

// A check that does not say "no such resource" is no proof the graph is gone.
func TestAFailedExistenceCheckDoesNotCreateASecondGraph(t *testing.T) {
	for name, failure := range map[string]struct {
		err  error
		code int
	}{
		"unreachable":         {errors.New("unable to reach permissions-v2"), 0},
		"failing":             {errors.New("permissions-v2 answered 500: boom"), http.StatusInternalServerError},
		"refused":             {errors.New("permissions-v2 answered 403: access denied"), http.StatusForbidden},
		"another kind of 404": {errors.New("permissions-v2 answered 404: unknown topic"), http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			store, shares, permissions, mirror := deletedGraphSetup()
			mirror.gone[testGraph] = true
			permissions.getErr[testGraph] = failure.err
			permissions.getCode[testGraph] = failure.code

			answer, logs := saveEnvironmentWithGraph(t, store, shares, permissions, mirror)

			assertGraphKept(t, store, mirror, answer)
			if !strings.Contains(logs, "unable to tell whether the graph of an environment still exists") {
				t.Errorf("expected the failed check as a warning, got %s", logs)
			}
		})
	}
}

// Only the refusal a deleted graph gets leads to the check; a repository that is
// down says nothing about the graph.
func TestAFailingRepositoryDoesNotLeadToTheCheck(t *testing.T) {
	store, shares, permissions, mirror := deletedGraphSetup()
	mirror.setErr, mirror.setCode = errors.New("device-repository is down"), http.StatusInternalServerError

	answer, _ := saveEnvironmentWithGraph(t, store, shares, permissions, mirror)

	if answer.ExternalGraphRef != testGraph || store.stored["env-1"].ExternalGraphRef != testGraph {
		t.Errorf("the ref has to be kept, got %q", store.stored["env-1"].ExternalGraphRef)
	}
	if permissions.sawTopic(graphsTopic) {
		t.Error("a failing repository must not lead to a check of the graph")
	}
}

func TestAFailingCreateOfTheReplacementKeepsTheRef(t *testing.T) {
	store, shares, permissions, mirror := deletedGraphSetup()
	mirror.gone[testGraph] = true
	mirror.createErr = errors.New("device-repository refused the create")

	answer, logs := saveEnvironmentWithGraph(t, store, shares, permissions, mirror)

	if answer.ExternalGraphRef != testGraph || store.stored["env-1"].ExternalGraphRef != testGraph {
		t.Errorf("a failed create has to keep the ref for the next save, got %q", store.stored["env-1"].ExternalGraphRef)
	}
	if !strings.Contains(logs, "unable to mirror an environment as a graph") {
		t.Errorf("expected the failed create as a warning, got %s", logs)
	}
}

func TestWithoutAPermissionsClientADeletedGraphIsNotReplaced(t *testing.T) {
	store, shares, _, mirror := deletedGraphSetup()
	mirror.gone[testGraph] = true

	answer, _ := saveEnvironmentWithGraph(t, store, shares, nil, mirror)

	assertGraphKept(t, store, mirror, answer)
}

// A graph that exists is written as before: one write to the stored ref and no
// check.
func TestAnExistingGraphIsWrittenOnceAndNotChecked(t *testing.T) {
	store, shares, permissions, mirror := deletedGraphSetup()
	permissions.ownGraph(testGraph, "user-a")
	mirror.stored[testGraph] = models.Graph{Id: testGraph}

	answer, _ := saveEnvironmentWithGraph(t, store, shares, permissions, mirror)

	assertGraphKept(t, store, mirror, answer)
	if permissions.sawTopic(graphsTopic) {
		t.Error("a graph that was written needs no check")
	}
}

// assertGraphKept: the save wrote to the stored ref only, created nothing and
// kept the ref.
func assertGraphKept(t *testing.T, store *fakeEnvironments, mirror *fakeGraphMirror, answer domain.Environment) {
	t.Helper()
	if store.stored["env-1"].ExternalGraphRef != testGraph || answer.ExternalGraphRef != testGraph {
		t.Errorf("the ref has to be kept, got %q", store.stored["env-1"].ExternalGraphRef)
	}
	if mirror.created != 10 {
		t.Errorf("no graph may be created, got %d", mirror.created-10)
	}
	if len(mirror.sent) != 1 || mirror.sent[0].Id != testGraph {
		t.Errorf("expected exactly one write to the stored graph, got %+v", mirror.sent)
	}
}
