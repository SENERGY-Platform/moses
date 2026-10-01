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

package runtime

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/scripthttp"
)

// noHttpGet binds the name in the reference runners, whose scripts never call it.
func noHttpGet(string) string { return "" }

// loopbackScriptHTTP is the production client plus the loopback interface, so a
// script reaches the httptest servers of a test.
func loopbackScriptHTTP(t *testing.T) *scripthttp.Client {
	t.Helper()
	client, err := scripthttp.New("", scripthttp.WithAddressCheck(func(addr netip.Addr) bool {
		return addr.Unmap().IsLoopback() || scripthttp.IsPublic(addr)
	}))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// sends records what a script handed to moses.service.send.
type sends struct {
	mux    sync.Mutex
	values []interface{}
}

func (this *sends) surface() map[string]interface{} {
	return map[string]interface{}{
		"service": map[string]interface{}{"send": func(v interface{}) {
			this.mux.Lock()
			defer this.mux.Unlock()
			this.values = append(this.values, v)
		}},
	}
}

func (this *sends) all() []interface{} {
	this.mux.Lock()
	defer this.mux.Unlock()
	return append([]interface{}{}, this.values...)
}

// stalledServer accepts a request and answers only when the test ends.
func stalledServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte("late"))
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	return server
}

func runHttpGetScript(t *testing.T, code string, timeout time.Duration, mux sync.Locker, client *scripthttp.Client) (*sends, chan error) {
	t.Helper()
	program, err := compileScript("t", code)
	if err != nil {
		t.Fatal(err)
	}
	sent := &sends{}
	done := make(chan error, 1)
	go func() {
		done <- runScriptInBraked(nil, nil, program, sent.surface(), timeout, mux, nil, "", "", nil, client)
	}()
	return sent, done
}

// A request to an endpoint that never answers ends with the run's timeout, and
// the run ends as a timeout: the statement around it never stores its empty answer.
func TestHttpGetEndsTheRunAtItsTimeout(t *testing.T) {
	server := stalledServer(t)
	timeout := 300 * time.Millisecond
	started := time.Now()
	sent, done := runHttpGetScript(t, fmt.Sprintf(`moses.service.send(httpGet(%q)); moses.service.send("after");`, server.URL), timeout, nil, loopbackScriptHTTP(t))
	select {
	case err := <-done:
		if !errors.Is(err, ErrScriptTimeout) {
			t.Fatalf("expected the script timeout, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run is still in httpGet long after its timeout")
	}
	if elapsed := time.Since(started); elapsed < timeout {
		t.Fatalf("the run ended after %v, before its timeout", elapsed)
	}
	if values := sent.all(); len(values) != 0 {
		t.Fatalf("the timed-out statement went on and sent %v", values)
	}
}

// The request itself ends the run: a timer callback that is late does not let
// the empty answer through.
func TestHttpGetTimeoutDoesNotWaitForTheTimer(t *testing.T) {
	timeoutCallbackDelay.Store(int64(time.Second))
	defer timeoutCallbackDelay.Store(0)
	server := stalledServer(t)
	sent, done := runHttpGetScript(t, fmt.Sprintf(`moses.service.send(httpGet(%q));`, server.URL), 200*time.Millisecond, nil, loopbackScriptHTTP(t))
	select {
	case err := <-done:
		if !errors.Is(err, ErrScriptTimeout) {
			t.Fatalf("expected the script timeout, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end")
	}
	if values := sent.all(); len(values) != 0 {
		t.Fatalf("the timed-out statement went on and sent %v", values)
	}
}

// The budget of a request is the run's, which starts once the environment's
// lock is held: the wait for the lock does not count against it.
func TestHttpGetBudgetStartsWithTheRun(t *testing.T) {
	timeout := time.Second
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(timeout / 2):
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte("body"))
	}))
	t.Cleanup(server.Close)
	mux := &sync.Mutex{}
	mux.Lock()
	sent, done := runHttpGetScript(t, fmt.Sprintf(`moses.service.send(httpGet(%q));`, server.URL), timeout, mux, loopbackScriptHTTP(t))
	time.Sleep(timeout * 3 / 4)
	mux.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected the run to succeed, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end")
	}
	if values := sent.all(); len(values) != 1 || values[0] != "body" {
		t.Fatalf("expected the body, got %v", values)
	}
}

// The control: an answer within the run is the body, and a request that fails
// for another reason is an empty string the run goes on with.
func TestHttpGetReturnsTheBodyOrAnEmptyString(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("body"))
	}))
	t.Cleanup(server.Close)
	closed := httptest.NewServer(http.NotFoundHandler())
	closedUrl := closed.URL
	closed.Close()
	code := fmt.Sprintf(`moses.service.send(httpGet(%q)); moses.service.send(httpGet(%q)); moses.service.send(httpGet("file:///etc/passwd"));`, server.URL, closedUrl)
	sent, done := runHttpGetScript(t, code, 5*time.Second, nil, loopbackScriptHTTP(t))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if values := sent.all(); len(values) != 3 || values[0] != "body" || values[1] != "" || values[2] != "" {
		t.Fatalf("expected the body and two empty strings, got %q", values)
	}
}

// What production wires refuses a server on 127.0.0.1, and so does a runtime
// without a client.
func TestARuntimesScriptCannotReachLoopback(t *testing.T) {
	hits := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("reached"))
	}))
	t.Cleanup(server.Close)
	strict, err := scripthttp.New("")
	if err != nil {
		t.Fatal(err)
	}
	if rt := New(testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), nil, newFakeHistoryJobs(), nil, nil, nil, strict); rt.scriptHTTP != strict {
		t.Fatal("New does not keep the client it is given")
	}
	code := fmt.Sprintf(`var got = httpGet(%q); moses.service.send(got === "" ? "refused" : got);`, server.URL)
	for name, client := range map[string]*scripthttp.Client{"production client": strict, "no client": nil} {
		publisher := &fakePublisher{}
		env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), code))
		startRuntimeWith(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher, client)
		if !waitFor(4*time.Second, func() bool { return len(publisher.forDevice(deviceRefOf("env-a"))) > 0 }) {
			t.Fatalf("%s: the channel did not publish", name)
		}
		for _, value := range publisher.forDevice(deviceRefOf("env-a")) {
			if fmt.Sprint(value) != "refused" {
				t.Fatalf("%s: expected the request to be refused, the script got %v", name, value)
			}
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the loopback server was reached %d times", hits.Load())
	}
}

// runExpiring runs code on the production api of a fresh environment, with a
// js timeout of 300ms and a client that reaches the test's servers.
func runExpiring(t *testing.T, code string) ([]interface{}, *environment, error) {
	t.Helper()
	const envId = "env-http-expire"
	def := testEnvironment(envId, scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf(envId), code))
	gen := newGeneration(def, nil)
	binding := gen.sensors[0]
	if binding.script == nil {
		t.Fatalf("expected the script to compile, got %v", binding.scriptErr)
	}
	env := &environment{id: envId}
	sent := &sends{}
	rt := &Runtime{jsTimeout: 300 * time.Millisecond}
	api := rt.jsApi(env, gen, binding, nil, func(value interface{}) {
		sent.mux.Lock()
		defer sent.mux.Unlock()
		sent.values = append(sent.values, value)
	}, time.Now())
	done := make(chan error, 1)
	go func() {
		done <- runScriptInBraked(nil, nil, binding.script, api, rt.jsTimeout, nil, nil, "", "", &env.sink, loopbackScriptHTTP(t))
	}()
	select {
	case err := <-done:
		return sent.all(), env, err
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end")
		return nil, nil, nil
	}
}

// goja checks the interrupt only between instructions, so a promise job or a
// getter that calls httpGet natively reaches a sink without one: the sinks of a
// run whose request missed the deadline write nothing, and the run is a timeout.
func TestASinkReachedNativelyAfterAnExpiredRequestWritesNothing(t *testing.T) {
	server := stalledServer(t)
	getter := fmt.Sprintf(`var o = {}; Object.defineProperty(o, "v", {get: httpGet.bind(null, %q), enumerable: true});`, server.URL)
	cases := map[string]string{
		"promise into send":      fmt.Sprintf(`Promise.resolve(%q).then(httpGet).then(moses.channel.send);`, server.URL),
		"getter into send":       getter + ` moses.channel.send(o);`,
		"getter into set":        getter + ` moses.asset.state.set("k", o);`,
		"getter into context":    getter + ` moses.environment.state.set("k", o);`,
		"promise into set":       fmt.Sprintf(`Promise.resolve(%q).then(httpGet).then(moses.asset.state.set.bind(null, "k"));`, server.URL),
		"promise into get seeds": fmt.Sprintf(`Promise.resolve(%q).then(httpGet).then(moses.asset.state.get.bind(null, "seeded"));`, server.URL),
	}
	for name, code := range cases {
		t.Run(name, func(t *testing.T) {
			sent, env, err := runExpiring(t, code)
			if !errors.Is(err, ErrScriptTimeout) {
				t.Errorf("expected the script timeout, got %v", err)
			}
			if len(sent) != 0 {
				t.Errorf("the expired run sent %v", sent)
			}
			for _, key := range []string{"k", "seeded"} {
				if _, stored := env.state.Assets[testAssetId][key]; stored {
					t.Errorf("the expired run stored %q on the asset", key)
				}
				if _, stored := env.state.Context[key]; stored {
					t.Errorf("the expired run stored %q in the context", key)
				}
			}
		})
	}
}

// The same through a started runtime: a channel whose promise or getter calls
// httpGet into send publishes nothing while its requests miss the deadline.
func TestARuntimePublishesNothingFromAnExpiredRequest(t *testing.T) {
	hits := &atomic.Int64{}
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	cfg := testConfig(time.Hour)
	cfg.JsTimeout = 200 * time.Millisecond
	publisher := &fakePublisher{}
	env := testEnvironment("env-a",
		scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), fmt.Sprintf(`Promise.resolve(%q).then(httpGet).then(moses.channel.send);`, server.URL)),
		scriptChannel("ch-2", domain.Sensor, 1, "urn:infai:ses:service:env-a-2", fmt.Sprintf(`var o = {}; Object.defineProperty(o, "v", {get: httpGet.bind(null, %q), enumerable: true}); moses.channel.send(o);`, server.URL)),
	)
	startRuntimeWith(t, cfg, newFakeEnvironments(env), newFakeStates(), publisher, loopbackScriptHTTP(t))
	if !waitFor(6*time.Second, func() bool { return hits.Load() >= 4 }) {
		t.Fatalf("the channels did not run, %d requests", hits.Load())
	}
	if published := publisher.forDevice(deviceRefOf("env-a")); len(published) != 0 {
		t.Fatalf("an expired run published %v", published)
	}
}
