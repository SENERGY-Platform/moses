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

package state

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A legacy httpGet to an endpoint that never answers must give up when the run
// times out and end the run like the interrupt: otto checks the interrupt only
// when it evaluates the next expression, so an empty answer would reach the
// rest of the statement and be stored.
func TestLegacyHttpGetEndsTheRunAtItsTimeout(t *testing.T) {
	store := newWorldStore()
	repo := crudTestRepo(t, store)
	gate := newScriptGate(t)
	world := repo.Worlds["w"]
	timeout := 300 * time.Millisecond
	done := make(chan error, 1)
	started := time.Now()
	go func() {
		done <- run(fmt.Sprintf(`moses.world.state.set("key", httpGet(%q));`, gate.url), repo.getJsWorldApi(world), timeout, world.mux, nil, "w", "c")
	}()
	select {
	case err := <-done:
		if err == nil || err.Error() != "Some code took to long" {
			t.Fatalf("expected the run to end with the timeout error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run is still stuck in httpGet long after its timeout")
	}
	if elapsed := time.Since(started); elapsed < timeout {
		t.Fatalf("the run ended after %v, before its timeout", elapsed)
	}
	for _, write := range store.history() {
		if _, ok := write.world.States["key"]; ok {
			t.Fatal("the statement that timed out stored its value")
		}
	}
	world.mux.Lock()
	_, set := world.States["key"]
	world.mux.Unlock()
	if set {
		t.Fatal("the statement that timed out set the state in memory")
	}
}

// A run's requests share the budget of its interrupt, which counts from the
// start of the run including its wait for the world: a run that queued behind
// another run of its world and then calls a slow endpoint ends with the timeout
// error once its own budget is spent, and stores nothing from that statement.
func TestLegacyHttpGetBudgetCountsFromTheRunsStart(t *testing.T) {
	store := newWorldStore()
	repo := crudTestRepo(t, store)
	world := repo.Worlds["w"]
	timeout := time.Second
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		//the endpoint's latency: within a budget counted from the lock, past one counted from the start
		select {
		case <-time.After(timeout / 2):
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte("body"))
	}))
	t.Cleanup(server.Close)
	//stands in for another run of the same world
	world.mux.Lock()
	done := make(chan error, 1)
	started := time.Now()
	runner := goSpawn(func() {
		done <- run(fmt.Sprintf(`moses.world.state.set("key", httpGet(%q));`, server.URL), repo.getJsWorldApi(world), timeout, world.mux, nil, "w", "c")
	})
	waitInStack(t, runner, "to the world mutex", inFrames("state.run(", "sync.(*Mutex).Lock("))
	//how long the other run holds the world, not an interleaving
	time.Sleep(time.Until(started.Add(timeout * 3 / 4)))
	world.mux.Unlock()
	select {
	case err := <-done:
		if err == nil || err.Error() != "Some code took to long" {
			t.Fatalf("expected the run to end with the timeout error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end")
	}
	for _, write := range store.history() {
		if _, ok := write.world.States["key"]; ok {
			t.Fatal("the statement that ran past its budget stored its value")
		}
	}
	world.mux.Lock()
	_, set := world.States["key"]
	world.mux.Unlock()
	if set {
		t.Fatal("the statement that ran past its budget set the state in memory")
	}
}

// The control: an endpoint that answers within the run still hands the script
// its body, and a request that fails for another reason still gives it an
// empty string without ending the run.
func TestLegacyHttpGetReturnsTheBodyOrAnEmptyString(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("body"))
	}))
	t.Cleanup(server.Close)
	refused := httptest.NewServer(http.NotFoundHandler())
	refusedUrl := refused.URL
	refused.Close()
	got := []string{}
	moses := map[string]interface{}{"got": func(value string) { got = append(got, value) }}
	code := fmt.Sprintf(`moses.got(httpGet(%q)); moses.got(httpGet(%q));`, server.URL, refusedUrl)
	if err := run(code, moses, time.Second, nil, nil, "w", "c"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "body" || got[1] != "" {
		t.Fatalf("expected the body and then an empty string, got %q", got)
	}
}

// Stop waits under the write lock for every running routine; a routine stuck
// in httpGet must not keep an update, and every api change and command behind
// it, waiting past the script timeout.
func TestAnUpdateWaitingOnAStuckHttpGetCompletes(t *testing.T) {
	store := newWorldStore()
	repo := crudTestRepo(t, store)
	//long enough that the update is seen waiting in Stop, short enough to bound the test
	repo.Config.JsTimeout = 2 * time.Second
	gate := newScriptGate(t)
	//taken first: DevGetWorld waits for the world's mutex, which the routine holds from its first tick on
	snapshot, _, err := repo.DevGetWorld("w")
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Name = "renamed"
	if _, _, _, err := repo.UpdateChangeRoutine(crudToken, UpdateChangeRoutineRequest{Id: "routine-world", Interval: 1, Code: fmt.Sprintf(`httpGet(%q);`, gate.url)}); err != nil {
		t.Fatal(err)
	}
	gate.waitEntered(t)
	done := make(chan error, 1)
	update := goSpawn(func() { done <- repo.DevUpdateWorld(snapshot) })
	waitInStack(t, update, "into Stop", inFrames(".(*StateRepo).Stop("))
	if err := awaitResult(t, done); err != nil {
		t.Fatal(err)
	}
	if world, _ := store.stored(t, "w"); world.Name != "renamed" {
		t.Fatalf("the update was not stored, the stored name is %q", world.Name)
	}
}

// A body cut off by the run's deadline ends the run like a request cut off
// before its answer; a body the server breaks off, or an address that is no
// url, gives the script an empty string and the run goes on.
func TestLegacyHttpGetBodyAndAddressFailures(t *testing.T) {
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(stalled.Close)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = buffered.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort")
		_ = buffered.Flush()
		_ = conn.Close()
	}))
	t.Cleanup(broken.Close)

	t.Run("body stalls past the deadline", func(t *testing.T) {
		got := []string{}
		moses := map[string]interface{}{"got": func(value string) { got = append(got, value) }}
		err := run(fmt.Sprintf(`moses.got(httpGet(%q));`, stalled.URL), moses, 300*time.Millisecond, nil, nil, "w", "c")
		if err == nil || err.Error() != "Some code took to long" {
			t.Fatalf("expected the timeout error, got %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("the statement went on with %q", got)
		}
	})
	t.Run("body broken off and no url", func(t *testing.T) {
		got := []string{}
		moses := map[string]interface{}{"got": func(value string) { got = append(got, value) }}
		code := fmt.Sprintf(`moses.got(httpGet(%q)); moses.got(httpGet("http://a b"));`, broken.URL)
		if err := run(code, moses, time.Second, nil, nil, "w", "c"); err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0] != "" || got[1] != "" {
			t.Fatalf("expected two empty strings, got %q", got)
		}
	})
}
