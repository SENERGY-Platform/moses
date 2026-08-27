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
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/repo"
	moses_runtime "github.com/SENERGY-Platform/moses/lib/runtime"
)

// GET /environments/{id}/state is the reading direction of the PATCH on the same
// path, and the shape of the two is the contract the editor is written against.

func storedEnvironmentFor(t *testing.T, store *fakeEnvironments, id string, owner string) {
	t.Helper()
	env := minimalEnvironment()
	env.Id, env.Owner = id, owner
	store.stored[id] = env
}

func readState(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	result := map[string]interface{}{}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestTheLiveStateIsReadInTheShapeThatIsWrittenBack(t *testing.T) {
	store := newFakeEnvironments()
	storedEnvironmentFor(t, store, "env-1", "user-a")
	asOf := time.Date(2026, 8, 27, 9, 41, 0, 0, time.UTC)
	notifier := &recordingNotifier{snapshot: moses_runtime.StateSnapshot{
		State: repo.StateChange{
			Context: map[string]interface{}{"outdoor": -7.5},
			Zones:   map[string]map[string]interface{}{"zone-1": {"temperature": 26.5}},
			Assets:  map[string]map[string]interface{}{"asset-1": {"rpm": 1450.0}},
		},
		AsOf: asOf,
	}}
	router := testRouterWithNotifier(store, notifier)

	resp := do(t, router, "GET", "/environments/env-1/state", "user-a", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	body := readState(t, resp.Body.Bytes())

	//the three members of a StateChange, at the top level and named exactly as
	//PATCH accepts them: an editor reads a value here and sends it straight back
	for _, member := range []string{"context", "zones", "assets"} {
		if _, ok := body[member]; !ok {
			t.Errorf("expected the answer to carry %q at the top level, got %v", member, body)
		}
	}
	if body["running"] != true {
		t.Errorf("expected running=true, got %v", body["running"])
	}
	if body["as_of"] != asOf.Format(time.RFC3339) {
		t.Errorf("expected as_of in RFC3339, got %v", body["as_of"])
	}

	//and it decodes back into the type the writing direction takes
	change := repo.StateChange{}
	if err := json.Unmarshal(resp.Body.Bytes(), &change); err != nil {
		t.Fatal(err)
	}
	if change.Context["outdoor"] != -7.5 || change.Zones["zone-1"]["temperature"] != 26.5 || change.Assets["asset-1"]["rpm"] != 1450.0 {
		t.Errorf("expected the answer to read back as a state change, got %#v", change)
	}
	if len(notifier.snapshotCalls) != 1 || notifier.snapshotCalls[0] != "env-1" {
		t.Errorf("expected exactly the environment from the path to be read, got %#v", notifier.snapshotCalls)
	}
}

// Not running is an answer, not an error: the editor builds its empty state from
// it, and another instance may well be running the environment.
func TestAnEnvironmentThatIsNotRunningAnswersWithRunningFalse(t *testing.T) {
	store := newFakeEnvironments()
	storedEnvironmentFor(t, store, "env-1", "user-a")
	notifier := &recordingNotifier{snapshotErr: repo.ErrNotRunning}
	router := testRouterWithNotifier(store, notifier)

	resp := do(t, router, "GET", "/environments/env-1/state", "user-a", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	body := readState(t, resp.Body.Bytes())
	if body["running"] != false {
		t.Errorf("expected running=false, got %v", body["running"])
	}
	for _, member := range []string{"context", "zones", "assets"} {
		if value, ok := body[member]; !ok || value != nil {
			t.Errorf("expected %q to carry no state, got %#v", member, value)
		}
	}
	if body["as_of"] == nil || body["as_of"] == "" {
		t.Errorf("expected as_of to say when the question was answered, got %v", body["as_of"])
	}
}

// A store only deployment has no live state, which is the same answer.
func TestAStoreOnlyInstanceAnswersWithRunningFalse(t *testing.T) {
	store := newFakeEnvironments()
	storedEnvironmentFor(t, store, "env-1", "user-a")

	resp := do(t, testRouter(store), "GET", "/environments/env-1/state", "user-a", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if readState(t, resp.Body.Bytes())["running"] != false {
		t.Errorf("expected running=false without a runtime, got %s", resp.Body.String())
	}
}

func TestReadingTheStateOfAnUnknownEnvironmentIs404(t *testing.T) {
	notifier := &recordingNotifier{}
	resp := do(t, testRouterWithNotifier(newFakeEnvironments(), notifier), "GET", "/environments/nope/state", "user-a", nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(notifier.snapshotCalls) != 0 {
		t.Errorf("the runtime must not be asked about an environment that does not exist, got %#v", notifier.snapshotCalls)
	}
}

// Ownership is decided on the stored document, like everywhere else here, and a
// caller without access must not reach the runtime at all.
func TestReadingTheStateOfSomebodyElsesEnvironmentIs404(t *testing.T) {
	store := newFakeEnvironments()
	storedEnvironmentFor(t, store, "env-1", "user-a")
	notifier := &recordingNotifier{}

	resp := do(t, testRouterWithNotifier(store, notifier), "GET", "/environments/env-1/state", "user-b", nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(notifier.snapshotCalls) != 0 {
		t.Errorf("the runtime must not be asked for a caller without access, got %#v", notifier.snapshotCalls)
	}
}

func TestAnAdminReadsTheStateOfAnyEnvironment(t *testing.T) {
	store := newFakeEnvironments()
	storedEnvironmentFor(t, store, "env-1", "user-a")
	notifier := &recordingNotifier{snapshot: moses_runtime.StateSnapshot{
		State: repo.StateChange{Context: map[string]interface{}{"outdoor": 1.0}},
		AsOf:  time.Now(),
	}}

	resp := doAsAdmin(t, testRouterWithNotifier(store, notifier), "GET", "/environments/env-1/state", "admin-user")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 for an admin, got %d: %s", resp.Code, resp.Body.String())
	}
	if readState(t, resp.Body.Bytes())["running"] != true {
		t.Errorf("expected running=true, got %s", resp.Body.String())
	}
}

func TestATokenWithoutASubjectCannotReadTheState(t *testing.T) {
	store := newFakeEnvironments()
	storedEnvironmentFor(t, store, "env-1", "user-a")
	resp := do(t, testRouter(store), "GET", "/environments/env-1/state", "", nil)
	if resp.Code == http.StatusOK {
		t.Fatalf("expected the read to be refused without a token, got %d: %s", resp.Code, resp.Body.String())
	}
}
