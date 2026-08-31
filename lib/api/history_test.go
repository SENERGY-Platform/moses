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

var testHistoryFrom = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

func historyBody() map[string]string {
	return map[string]string{"from": testHistoryFrom.Format(time.RFC3339)}
}

func TestStartingAHistoryRunHandsTheInstantToTheRuntime(t *testing.T) {
	notifier := &recordingNotifier{}
	router := testRouterWithNotifier(backfillStore("user-a"), notifier)

	resp := do(t, router, "POST", "/environments/env-1/history", "user-a", historyBody())
	if resp.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(notifier.histories) != 1 {
		t.Fatalf("expected one history run to be started, got %d", len(notifier.histories))
	}
	if !notifier.histories[0].From.Equal(testHistoryFrom) {
		t.Errorf("the instant arrived as %v", notifier.histories[0].From)
	}

	body := moses_runtime.HistoryStatus{}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.EnvironmentId != "env-1" || body.State != moses_runtime.HistoryRunning {
		t.Errorf("expected the running run of env-1 in the response, got %#v", body)
	}
}

// TestAHistoryRunOfSomebodyElsesEnvironmentIsNotStarted: the ownership check has
// to come before the runtime is asked, or a caller without access could both
// learn that the environment exists and suspend its simulation for hours.
func TestAHistoryRunOfSomebodyElsesEnvironmentIsNotStarted(t *testing.T) {
	notifier := &recordingNotifier{}
	router := testRouterWithNotifier(backfillStore("user-a"), notifier)

	for _, method := range []string{"POST", "GET", "DELETE"} {
		var body interface{}
		if method == "POST" {
			body = historyBody()
		}
		resp := do(t, router, method, "/environments/env-1/history", "user-b", body)
		if resp.Code != http.StatusNotFound {
			t.Errorf("expected 404 for %s by a stranger, got %d: %s", method, resp.Code, resp.Body.String())
		}
	}
	if len(notifier.histories) != 0 {
		t.Errorf("a stranger started %d history runs", len(notifier.histories))
	}
	if notifier.historyStatusCall != 0 || len(notifier.historyCancels) != 0 {
		t.Errorf("a stranger reached the runtime: %d reads, %d aborts", notifier.historyStatusCall, len(notifier.historyCancels))
	}

	resp := do(t, router, "POST", "/environments/nothing/history", "user-a", historyBody())
	if resp.Code != http.StatusNotFound {
		t.Errorf("expected 404 for an unknown environment, got %d", resp.Code)
	}
}

func TestTheHistoryEndpointsMapEveryRuntimeAnswer(t *testing.T) {
	for name, testCase := range map[string]struct {
		err  error
		want int
	}{
		"a window it will not serve": {&moses_runtime.HistoryRangeError{Reason: "from has to lie in the past"}, http.StatusBadRequest},
		"a run already going":        {moses_runtime.ErrHistoryRunning, http.StatusConflict},
		"a backfill in the way":      {moses_runtime.ErrBackfillRunning, http.StatusConflict},
		"an environment not held":    {repo.ErrNotRunning, http.StatusNotFound},
		"no runtime at all":          {ErrNoRuntime, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			notifier := &recordingNotifier{historyStartErr: testCase.err}
			router := testRouterWithNotifier(backfillStore("user-a"), notifier)
			resp := do(t, router, "POST", "/environments/env-1/history", "user-a", historyBody())
			if resp.Code != testCase.want {
				t.Errorf("expected %d, got %d: %s", testCase.want, resp.Code, resp.Body.String())
			}
		})
	}
}

func TestAnUnreadableHistoryWindowIsRefused(t *testing.T) {
	notifier := &recordingNotifier{}
	router := testRouterWithNotifier(backfillStore("user-a"), notifier)

	for name, body := range map[string]interface{}{
		"not a timestamp": map[string]string{"from": "yesterday"},
		"not an object":   []string{"a", "b"},
	} {
		t.Run(name, func(t *testing.T) {
			resp := do(t, router, "POST", "/environments/env-1/history", "user-a", body)
			if resp.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d: %s", resp.Code, resp.Body.String())
			}
		})
	}
	if len(notifier.histories) != 0 {
		t.Errorf("an unreadable window started %d history runs", len(notifier.histories))
	}
}

func TestReadingTheHistoryRunOfAnEnvironment(t *testing.T) {
	finished := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	position := time.Date(2026, 8, 27, 9, 59, 0, 0, time.UTC)
	notifier := &recordingNotifier{historyStatus: moses_runtime.HistoryStatus{
		EnvironmentId: "env-1",
		State:         moses_runtime.HistoryDone,
		From:          testHistoryFrom,
		To:            finished,
		FinishedAt:    &finished,
		Position:      &position,
		Published:     1234,
		Failed:        2,
		LastError:     "the platform refused this reading",
		Channels: []moses_runtime.HistoryChannelStatus{
			{ChannelId: "ch-1", Publishable: true, Published: 1234, Failed: 2},
			{ChannelId: "ch-2", Reason: "the channel has no platform service, so a reading has nowhere to go", Silent: 60},
		},
	}}
	router := testRouterWithNotifier(backfillStore("user-a"), notifier)

	resp := do(t, router, "GET", "/environments/env-1/history", "user-a", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	body := moses_runtime.HistoryStatus{}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Published != 1234 || body.Failed != 2 {
		t.Errorf("expected the counters back, got %#v", body)
	}
	if body.LastError == "" {
		t.Error("the publish failure has to survive the serialisation, or a caller cannot tell why readings are missing")
	}
	if body.Position == nil || !body.Position.Equal(position) {
		t.Errorf("expected the virtual position back, got %v", body.Position)
	}
	if body.FinishedAt == nil || !body.FinishedAt.Equal(finished) {
		t.Errorf("expected the finish time back, got %v", body.FinishedAt)
	}
	if len(body.Channels) != 2 {
		t.Fatalf("expected both channels back, got %#v", body.Channels)
	}
	if body.Channels[1].Reason == "" || body.Channels[1].Publishable {
		t.Error("the reason a channel published nothing has to survive the serialisation, or a run that published nothing looks like a broken environment")
	}
}

func TestAbortingTheHistoryRunOfAnEnvironment(t *testing.T) {
	notifier := &recordingNotifier{historyStatus: moses_runtime.HistoryStatus{
		EnvironmentId: "env-1", State: moses_runtime.HistoryRunning,
	}}
	router := testRouterWithNotifier(backfillStore("user-a"), notifier)

	resp := do(t, router, "DELETE", "/environments/env-1/history", "user-a", nil)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(notifier.historyCancels) != 1 || notifier.historyCancels[0] != "env-1" {
		t.Errorf("expected exactly env-1 to be aborted, got %#v", notifier.historyCancels)
	}
}

func TestAnUnknownHistoryRunIsNotFound(t *testing.T) {
	notifier := &recordingNotifier{historyStatusErr: moses_runtime.ErrNoHistory, historyCancelErr: moses_runtime.ErrNoHistory}
	router := testRouterWithNotifier(backfillStore("user-a"), notifier)
	for _, method := range []string{"GET", "DELETE"} {
		if resp := do(t, router, method, "/environments/env-1/history", "user-a", nil); resp.Code != http.StatusNotFound {
			t.Errorf("expected 404 for %s, got %d: %s", method, resp.Code, resp.Body.String())
		}
	}
}

// TestTheHistoryEndpointsSurviveAStoreOnlyDeployment: an instance serving the
// store without a runtime must answer rather than panic on the nil notifier.
func TestTheHistoryEndpointsSurviveAStoreOnlyDeployment(t *testing.T) {
	router := testRouter(backfillStore("user-a"))
	if resp := do(t, router, "POST", "/environments/env-1/history", "user-a", historyBody()); resp.Code != http.StatusNotFound {
		t.Errorf("expected 404 without a runtime, got %d", resp.Code)
	}
	for _, method := range []string{"GET", "DELETE"} {
		if resp := do(t, router, method, "/environments/env-1/history", "user-a", nil); resp.Code != http.StatusNotFound {
			t.Errorf("expected 404 for %s without a runtime, got %d", method, resp.Code)
		}
	}
}

func TestTheHistoryEndpointsNeedASubject(t *testing.T) {
	router := testRouterWithNotifier(backfillStore("user-a"), &recordingNotifier{})
	if resp := do(t, router, "POST", "/environments/env-1/history", "", historyBody()); resp.Code != http.StatusBadRequest {
		t.Errorf("expected 400 without a token, got %d", resp.Code)
	}
	for _, method := range []string{"GET", "DELETE"} {
		if resp := do(t, router, method, "/environments/env-1/history", "", nil); resp.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for %s without a token, got %d", method, resp.Code)
		}
	}
}

// TestABackfillIsRefusedWhileAHistoryRunOwnsTheEnvironment: the two exclude each
// other in the runtime, and the api has to turn that into a 409 rather than the
// 500 an unmapped error would give.
func TestABackfillIsRefusedWhileAHistoryRunOwnsTheEnvironment(t *testing.T) {
	notifier := &recordingNotifier{startErr: moses_runtime.ErrHistoryRunning}
	router := testRouterWithNotifier(backfillStore("user-a"), notifier)
	resp := do(t, router, "POST", "/environments/env-1/backfill", "user-a", backfillBody())
	if resp.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", resp.Code, resp.Body.String())
	}
}

// TestSettingTheStateIsRefusedWhileAHistoryRunOwnsTheEnvironment: 409 and not
// 404 - the environment is here and known, it just stands at a past instant, and
// the change would be thrown away with the state the run replaces.
func TestSettingTheStateIsRefusedWhileAHistoryRunOwnsTheEnvironment(t *testing.T) {
	store := newFakeEnvironments()
	storedEnvironmentFor(t, store, "env-1", "user-a")
	notifier := &recordingNotifier{setErr: moses_runtime.ErrHistoryRunning}
	router := testRouterWithNotifier(store, notifier)

	body := map[string]interface{}{"context": map[string]interface{}{"outdoor": -7.5}}
	resp := do(t, router, "PATCH", "/environments/env-1/state", "user-a", body)
	if resp.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", resp.Code, resp.Body.String())
	}
	if resp.Body.String() == "" {
		t.Error("the answer has to say why, or a caller cannot tell it from a version conflict")
	}
}

// TestReadingTheStateWhileAHistoryRunOwnsTheEnvironment: a read has an answer -
// that there is no live state and why - rather than a 409. An editor shows the
// run instead of an error.
func TestReadingTheStateWhileAHistoryRunOwnsTheEnvironment(t *testing.T) {
	store := newFakeEnvironments()
	storedEnvironmentFor(t, store, "env-1", "user-a")
	notifier := &recordingNotifier{snapshotErr: moses_runtime.ErrHistoryRunning}
	router := testRouterWithNotifier(store, notifier)

	resp := do(t, router, "GET", "/environments/env-1/state", "user-a", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	body := readState(t, resp.Body.Bytes())
	if body["running"] != false {
		t.Errorf("expected running=false, got %v", body["running"])
	}
	if body["history_running"] != true {
		t.Errorf("expected history_running=true, got %v", body["history_running"])
	}
}

// TestReadingTheStateOfAnEnvironmentWithoutAHistoryRunSaysNothingAboutOne: the
// flag is omitted when it is false, so an editor written before the mode existed
// keeps working unchanged.
func TestReadingTheStateOfAnEnvironmentWithoutAHistoryRunSaysNothingAboutOne(t *testing.T) {
	store := newFakeEnvironments()
	storedEnvironmentFor(t, store, "env-1", "user-a")
	notifier := &recordingNotifier{snapshotErr: repo.ErrNotRunning}
	router := testRouterWithNotifier(store, notifier)

	resp := do(t, router, "GET", "/environments/env-1/state", "user-a", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if _, present := readState(t, resp.Body.Bytes())["history_running"]; present {
		t.Error("history_running has to be absent when no run owns the environment")
	}
}
