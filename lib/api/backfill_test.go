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

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	moses_runtime "github.com/SENERGY-Platform/moses/lib/runtime"
)

var (
	testBackfillFrom = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	testBackfillTo   = time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
)

func backfillStore(owner string) *fakeEnvironments {
	store := newFakeEnvironments()
	store.stored["env-1"] = domain.Environment{Id: "env-1", Name: "env-1", Owner: owner}
	return store
}

func backfillBody() map[string]string {
	return map[string]string{
		"from": testBackfillFrom.Format(time.RFC3339),
		"to":   testBackfillTo.Format(time.RFC3339),
	}
}

func TestStartingABackfillHandsTheWindowToTheRuntime(t *testing.T) {
	notifier := &recordingNotifier{}
	router := testRouterWithNotifier(backfillStore("user-a"), notifier)

	resp := do(t, router, "POST", "/environments/env-1/backfill", "user-a", backfillBody())
	if resp.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(notifier.backfills) != 1 {
		t.Fatalf("expected one backfill to be started, got %d", len(notifier.backfills))
	}
	started := notifier.backfills[0]
	if !started.From.Equal(testBackfillFrom) || !started.To.Equal(testBackfillTo) {
		t.Errorf("the window arrived as %v..%v", started.From, started.To)
	}

	body := moses_runtime.BackfillStatus{}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.EnvironmentId != "env-1" || body.State != moses_runtime.BackfillRunning {
		t.Errorf("expected the running job of env-1 in the response, got %#v", body)
	}
}

// TestABackfillOfSomebodyElsesEnvironmentIsNotStarted: the ownership check has
// to come before the runtime is asked, or a caller without access could both
// learn that the environment exists and set a job going on it.
func TestABackfillOfSomebodyElsesEnvironmentIsNotStarted(t *testing.T) {
	notifier := &recordingNotifier{}
	router := testRouterWithNotifier(backfillStore("user-a"), notifier)

	for _, method := range []string{"POST", "GET"} {
		var body interface{}
		if method == "POST" {
			body = backfillBody()
		}
		resp := do(t, router, method, "/environments/env-1/backfill", "user-b", body)
		if resp.Code != http.StatusNotFound {
			t.Errorf("expected 404 for %s by a stranger, got %d: %s", method, resp.Code, resp.Body.String())
		}
	}
	if len(notifier.backfills) != 0 {
		t.Errorf("a stranger started %d backfills", len(notifier.backfills))
	}
	if notifier.statusCall != 0 {
		t.Errorf("a stranger read the status %d times", notifier.statusCall)
	}

	//and an environment that does not exist is the same answer
	resp := do(t, router, "POST", "/environments/nothing/backfill", "user-a", backfillBody())
	if resp.Code != http.StatusNotFound {
		t.Errorf("expected 404 for an unknown environment, got %d", resp.Code)
	}
}

func TestTheBackfillEndpointsMapEveryRuntimeAnswer(t *testing.T) {
	for name, testCase := range map[string]struct {
		err  error
		want int
	}{
		"a window it will not serve": {&moses_runtime.BackfillRangeError{Reason: "the window is empty"}, http.StatusBadRequest},
		"a job already running":      {moses_runtime.ErrBackfillRunning, http.StatusConflict},
		"an environment not held":    {repo.ErrNotRunning, http.StatusNotFound},
		"no runtime at all":          {ErrNoRuntime, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			notifier := &recordingNotifier{startErr: testCase.err}
			router := testRouterWithNotifier(backfillStore("user-a"), notifier)
			resp := do(t, router, "POST", "/environments/env-1/backfill", "user-a", backfillBody())
			if resp.Code != testCase.want {
				t.Errorf("expected %d, got %d: %s", testCase.want, resp.Code, resp.Body.String())
			}
		})
	}
}

func TestAnUnreadableBackfillWindowIsRefused(t *testing.T) {
	notifier := &recordingNotifier{}
	router := testRouterWithNotifier(backfillStore("user-a"), notifier)

	for name, body := range map[string]interface{}{
		"not a timestamp": map[string]string{"from": "yesterday", "to": "today"},
		"not an object":   []string{"a", "b"},
	} {
		t.Run(name, func(t *testing.T) {
			resp := do(t, router, "POST", "/environments/env-1/backfill", "user-a", body)
			if resp.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d: %s", resp.Code, resp.Body.String())
			}
		})
	}
	if len(notifier.backfills) != 0 {
		t.Errorf("an unreadable window started %d backfills", len(notifier.backfills))
	}
}

func TestReadingTheBackfillOfAnEnvironment(t *testing.T) {
	finished := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	notifier := &recordingNotifier{status: moses_runtime.BackfillStatus{
		EnvironmentId: "env-1",
		State:         moses_runtime.BackfillDone,
		From:          testBackfillFrom,
		To:            testBackfillTo,
		FinishedAt:    &finished,
		ChannelsTotal: 2,
		ChannelsDone:  2,
		Published:     1234,
		Channels: []moses_runtime.BackfillChannelStatus{
			{ChannelId: "ch-1", Backfillable: true, Published: 1234},
			{ChannelId: "ch-2", SkipReason: "a script source is stateful"},
		},
	}}
	router := testRouterWithNotifier(backfillStore("user-a"), notifier)

	resp := do(t, router, "GET", "/environments/env-1/backfill", "user-a", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	body := moses_runtime.BackfillStatus{}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Published != 1234 || len(body.Channels) != 2 {
		t.Fatalf("expected the whole status back, got %#v", body)
	}
	if body.Channels[1].SkipReason == "" {
		t.Error("the skip reason of a channel has to survive the serialisation, or a caller cannot tell why nothing appeared")
	}
	if body.FinishedAt == nil || !body.FinishedAt.Equal(finished) {
		t.Errorf("expected the finish time back, got %v", body.FinishedAt)
	}
}

func TestAnUnknownBackfillIsNotFound(t *testing.T) {
	notifier := &recordingNotifier{statusErr: moses_runtime.ErrNoBackfill}
	router := testRouterWithNotifier(backfillStore("user-a"), notifier)
	resp := do(t, router, "GET", "/environments/env-1/backfill", "user-a", nil)
	if resp.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", resp.Code, resp.Body.String())
	}
}

// TestTheBackfillEndpointsSurviveAStoreOnlyDeployment: an instance serving the
// store without a runtime must answer rather than panic on the nil notifier.
func TestTheBackfillEndpointsSurviveAStoreOnlyDeployment(t *testing.T) {
	router := testRouter(backfillStore("user-a"))
	if resp := do(t, router, "POST", "/environments/env-1/backfill", "user-a", backfillBody()); resp.Code != http.StatusNotFound {
		t.Errorf("expected 404 without a runtime, got %d", resp.Code)
	}
	if resp := do(t, router, "GET", "/environments/env-1/backfill", "user-a", nil); resp.Code != http.StatusNotFound {
		t.Errorf("expected 404 without a runtime, got %d", resp.Code)
	}
}

func TestTheBackfillEndpointsNeedASubject(t *testing.T) {
	router := testRouterWithNotifier(backfillStore("user-a"), &recordingNotifier{})
	if resp := do(t, router, "POST", "/environments/env-1/backfill", "", backfillBody()); resp.Code != http.StatusBadRequest {
		t.Errorf("expected 400 without a token, got %d", resp.Code)
	}
	if resp := do(t, router, "GET", "/environments/env-1/backfill", "", nil); resp.Code != http.StatusBadRequest {
		t.Errorf("expected 400 without a token, got %d", resp.Code)
	}
}
