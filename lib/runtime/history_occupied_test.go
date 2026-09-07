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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/timeseries"
	"github.com/SENERGY-Platform/moses/lib/util"
)

// occupancyEnvironment is two assets with a platform device each: the machine
// with two publishing channels and one that carries no time path, the oven with
// one. That is what tells "one entry per device" from "one entry per channel",
// and what tells a channel that is asked about from one that cannot publish.
func occupancyEnvironment(id string) domain.Environment {
	//a one second channel each, so the live simulation is visibly running while
	//a refusal is being decided
	def := testEnvironment(id,
		profileChannel("ch-a1", serviceRefOf(id)+"-a1", 1, flatProfile(230, 0)),
		profileChannel("ch-a2", serviceRefOf(id)+"-a2", 3600, flatProfile(231, 0)),
		profileChannel("ch-silent", serviceRefOf(id)+"-silent", 3600, flatProfile(232, 0)))
	oven := def.Zones[0].Assets[0]
	oven.Id = "asset-2"
	oven.Name = "oven"
	oven.ExternalRef = deviceRefOf(id) + "-2"
	//its own map: the copy above shares the machine's, and two assets writing
	//into one initial state would not be a document the api stores
	oven.InitialStates = map[string]interface{}{}
	oven.Channels = []domain.Channel{profileChannel("ch-b1", serviceRefOf(id)+"-b1", 1, flatProfile(400, 0))}
	def.Zones[0].Assets = append(def.Zones[0].Assets, oven)
	return def
}

// occupancyPublisher answers with a time shape for every service except the one
// channel that is meant to be unpublishable.
func occupancyPublisher(id string) *fakePublisher {
	return &fakePublisher{shapeErr: map[string]error{serviceRefOf(id) + "-silent": devices.ErrNoTimePath}}
}

// startOccupancyRuntime builds a runtime whose history engine is the fake one and
// lets the test wire the wrapper before the start, the way New does.
func startOccupancyRuntime(t *testing.T, def domain.Environment, publisher *fakePublisher, jobs *fakeHistoryJobs, engine historyEngineFunc, prepare func(rt *Runtime)) *Runtime {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, jobs, publisher)
	if engine != nil {
		rt.historyEngine = engine
	}
	if prepare != nil {
		prepare(rt)
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("unable to start the runtime: %v", err)
	}
	t.Cleanup(rt.Stop)
	return rt
}

// withWrapper is the ordinary wiring: a fetcher and the owner's token.
func withWrapper(fetcher *fakeFetcher) func(rt *Runtime) {
	return func(rt *Runtime) {
		rt.fetcher = fetcher
		rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }
	}
}

// TestAHistoryRunIsRefusedWhereTheFirstDayAlreadyHoldsReadings: the rows a run
// writes cannot be deleted again, so a window that already holds readings is
// named rather than written into a second time - and it is named before anything
// of the live simulation is stopped for it.
func TestAHistoryRunIsRefusedWhereTheFirstDayAlreadyHoldsReadings(t *testing.T) {
	const id = "env-hist-occupied"
	engine := newFakeHistoryEngine()
	close(engine.release)
	publisher := occupancyPublisher(id)
	//both channels of the machine and the one of the oven hold readings
	fetcher := &fakeFetcher{occupied: map[string]bool{
		serviceRefOf(id) + "-a1": true,
		serviceRefOf(id) + "-a2": true,
		serviceRefOf(id) + "-b1": true,
	}}
	jobs := newFakeHistoryJobs()
	rt := startOccupancyRuntime(t, occupancyEnvironment(id), publisher, jobs, engine.run, withWrapper(fetcher))

	//a value of the live simulation, which a run would discard with the rest of
	//the state before it computed anything
	rt.mux.RLock()
	env := rt.envs[id]
	rt.mux.RUnlock()
	if env == nil {
		t.Fatal("the environment is not running")
	}
	env.mux.Lock()
	env.assetStates(testAssetId)["marker"] = 4711.0
	env.mux.Unlock()

	//not aligned to a millisecond, so the truncation of the window is visible in
	//what the wrapper is asked about
	from := time.Now().Add(-time.Hour).Truncate(time.Millisecond).Add(400 * time.Microsecond)
	_, err := rt.StartHistory(id, from, false, "")
	occupied := &HistoryOccupiedError{}
	if !errors.As(err, &occupied) {
		t.Fatalf("expected a HistoryOccupiedError, got %v", err)
	}
	if len(occupied.Devices) != 2 {
		t.Fatalf("expected the two devices once each, got %#v", occupied.Devices)
	}
	want := []OccupiedDevice{
		{DeviceId: deviceRefOf(id), Name: "machine"},
		{DeviceId: deviceRefOf(id) + "-2", Name: "oven"},
	}
	for i, device := range occupied.Devices {
		if device != want[i] {
			t.Errorf("device %d is %#v, expected %#v", i, device, want[i])
		}
	}
	for _, fragment := range []string{deviceRefOf(id), "machine", "oven"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("the error has to name %q, it reads %q", fragment, err.Error())
		}
	}

	//three publishable channels are asked about, the unpublishable one is not
	checks := fetcher.occupancyChecks()
	if len(checks) != 3 {
		t.Fatalf("expected one query per publishable channel, got %d: %#v", len(checks), checks)
	}
	asked := map[string]occupancyCheck{}
	for _, check := range checks {
		asked[check.serviceId] = check
	}
	if _, silent := asked[serviceRefOf(id)+"-silent"]; silent {
		t.Error("a channel that cannot publish was asked about anyway")
	}
	for _, service := range []string{"-a1", "-a2", "-b1"} {
		check, found := asked[serviceRefOf(id)+service]
		if !found {
			t.Errorf("the channel on %v was never asked about", service)
			continue
		}
		//the window is exactly the first day from the truncated start
		if !check.start.Equal(from.Truncate(time.Millisecond)) {
			t.Errorf("%v was asked about %v, expected the window start %v", service, check.start, from.Truncate(time.Millisecond))
		}
		if !check.end.Equal(from.Truncate(time.Millisecond).Add(24 * time.Hour)) {
			t.Errorf("%v was asked about a window ending %v, expected the first day from %v",
				service, check.end, from.Truncate(time.Millisecond))
		}
		if got := check.end.Sub(check.start); got != 24*time.Hour {
			t.Errorf("%v was asked about a window of %v, expected 24h", service, got)
		}
		//the owner's token, as the platform origin uses it, and the flattened
		//value column the ingestion writes
		if check.token != "Bearer token-for-test-owner" {
			t.Errorf("%v was asked with %q", service, check.token)
		}
		if check.column != "root.value" {
			t.Errorf("%v was asked about the column %q", service, check.column)
		}
	}

	//and nothing was started, stored or stopped for the refused run
	if engine.callCount() != 0 {
		t.Error("the refused run still started the engine")
	}
	if _, err = rt.HistoryStatusOf(id); !errors.Is(err, ErrNoHistory) {
		t.Errorf("the refused run was left in the registry: %v", err)
	}
	if stored := jobs.savesOf(id); len(stored) != 0 {
		t.Errorf("the refused run was written to the store: %#v", stored)
	}
	env.mux.Lock()
	marker, _ := asFloat(env.assetStates(testAssetId)["marker"])
	underHistory := env.underHistory
	env.mux.Unlock()
	if marker != 4711 {
		t.Errorf("the live state was discarded for a run that never started, the marker is %v", marker)
	}
	if underHistory {
		t.Error("the environment was taken away from the live simulation")
	}
	before := publisher.count()
	if !waitFor(4*time.Second, func() bool { return publisher.count() > before }) {
		t.Error("the live simulation stopped over a run that was refused")
	}
}

// TestForceStartsAHistoryRunOverAnOccupiedWindow: the demo setup service rewrites
// the same window on purpose, and the check is not asked at all for such a run -
// a query per channel is not worth making for an answer nobody reads.
func TestForceStartsAHistoryRunOverAnOccupiedWindow(t *testing.T) {
	const id = "env-hist-forced"
	engine := newFakeHistoryEngine()
	fetcher := &fakeFetcher{occupied: map[string]bool{
		serviceRefOf(id) + "-a1": true,
		serviceRefOf(id) + "-b1": true,
	}}
	rt := startOccupancyRuntime(t, occupancyEnvironment(id), occupancyPublisher(id), newFakeHistoryJobs(), engine.run, withWrapper(fetcher))

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), true, ""); err != nil {
		t.Fatalf("a forced run over an occupied window has to start, got %v", err)
	}
	<-engine.entered
	close(engine.release)
	status := waitForHistory(t, rt, id)
	if status.State != HistoryDone {
		t.Errorf("expected the forced run to finish, it is %v (%v)", status.State, status.Error)
	}
	if checks := fetcher.occupancyChecks(); len(checks) != 0 {
		t.Errorf("a forced run asked the wrapper anyway: %#v", checks)
	}
}

// TestAHistoryRunStartsOnAFreeWindow is the other answer of the same check: a
// window nothing wrote into is not in the way of anything.
func TestAHistoryRunStartsOnAFreeWindow(t *testing.T) {
	const id = "env-hist-free"
	engine := newFakeHistoryEngine()
	fetcher := &fakeFetcher{}
	rt := startOccupancyRuntime(t, occupancyEnvironment(id), occupancyPublisher(id), newFakeHistoryJobs(), engine.run, withWrapper(fetcher))

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("a run over a free window has to start, got %v", err)
	}
	<-engine.entered
	close(engine.release)
	if status := waitForHistory(t, rt, id); status.State != HistoryDone {
		t.Errorf("expected the run to finish, it is %v (%v)", status.State, status.Error)
	}
	if checks := fetcher.occupancyChecks(); len(checks) != 3 {
		t.Errorf("expected one query per publishable channel, got %d", len(checks))
	}
}

// TestWithoutAWrapperTheHistoryWindowIsNotChecked: the check is a convenience of
// a deployment that has a timescale-wrapper, and a deployment without one has to
// keep running rather than losing the endpoint - but it says so, once, or a
// missing configuration would look like an empty timescale.
func TestWithoutAWrapperTheHistoryWindowIsNotChecked(t *testing.T) {
	const message = "the history window is not checked against the timescale"
	for name, prepare := range map[string]func(rt *Runtime){
		"no fetcher configured": func(rt *Runtime) {
			rt.ownerToken = func(userId string) (string, error) { return "Bearer t", nil }
		},
		"no token exchange configured": func(rt *Runtime) {
			rt.fetcher = &fakeFetcher{occupied: map[string]bool{}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			id := "env-hist-nowrapper-" + fmt.Sprint(len(name))
			log := &recordingWriter{}
			previous := util.Logger
			util.Logger = slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelInfo}))
			t.Cleanup(func() { util.Logger = previous })

			engine := newFakeHistoryEngine()
			rt := startOccupancyRuntime(t, occupancyEnvironment(id), occupancyPublisher(id), newFakeHistoryJobs(), engine.run, prepare)

			if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
				t.Fatalf("expected the run to start unchecked, got %v", err)
			}
			<-engine.entered
			close(engine.release)
			waitForHistory(t, rt, id)
			if lines := log.count(message); lines != 1 {
				t.Errorf("expected one line saying the window was not checked, got %d", lines)
			}
		})
	}
}

// TestAWrapperErrorRefusesTheHistoryRun: the check matters exactly where
// something was written before, so an answer that did not arrive is not the same
// as "the window is free". The refusal is neither a range nor an occupancy
// error, which is what makes the api answer 500 rather than 400 or 409.
func TestAWrapperErrorRefusesTheHistoryRun(t *testing.T) {
	const id = "env-hist-wrapper-broken"
	broken := errors.New("the timescale-wrapper answered 503")
	engine := newFakeHistoryEngine()
	publisher := occupancyPublisher(id)
	fetcher := &fakeFetcher{readingsErr: broken}
	jobs := newFakeHistoryJobs()
	rt := startOccupancyRuntime(t, occupancyEnvironment(id), publisher, jobs, engine.run, withWrapper(fetcher))

	_, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, "")
	if !errors.Is(err, broken) {
		t.Fatalf("expected the wrapper's error to refuse the run, got %v", err)
	}
	rangeError := &HistoryRangeError{}
	occupied := &HistoryOccupiedError{}
	if errors.As(err, &rangeError) || errors.As(err, &occupied) {
		t.Errorf("a wrapper that failed must not be reported as a window problem: %v", err)
	}
	if engine.callCount() != 0 {
		t.Error("the run was started although the window could not be checked")
	}
	if _, err = rt.HistoryStatusOf(id); !errors.Is(err, ErrNoHistory) {
		t.Errorf("the refused run was left in the registry: %v", err)
	}
	if stored := jobs.savesOf(id); len(stored) != 0 {
		t.Errorf("the refused run was written to the store: %#v", stored)
	}

	rt.mux.RLock()
	env := rt.envs[id]
	rt.mux.RUnlock()
	if env == nil {
		t.Fatal("the environment is gone")
	}
	env.mux.Lock()
	underHistory := env.underHistory
	env.mux.Unlock()
	if underHistory {
		t.Error("the environment was taken away for a run that was refused")
	}
	before := publisher.count()
	if !waitFor(4*time.Second, func() bool { return publisher.count() > before }) {
		t.Error("the live simulation stopped over a run that was refused")
	}

	//and a forced run starts although the wrapper is still broken
	if _, err = rt.StartHistory(id, time.Now().Add(-time.Hour), true, ""); err != nil {
		t.Errorf("a forced run has to start without the wrapper being asked, got %v", err)
	}
	<-engine.entered
	close(engine.release)
	waitForHistory(t, rt, id)
}

// TestTheOccupancyCheckKeepsItsFanOutBounded: one query per publishable channel
// of a site the size of the demonstrator is dozens of them, and a caller waits
// on the POST for the answer - so they go out in batches rather than all at
// once.
func TestTheOccupancyCheckKeepsItsFanOutBounded(t *testing.T) {
	const id = "env-hist-fanout"
	channels := []domain.Channel{}
	for i := 0; i < 3*historyOccupiedQueries; i++ {
		//hourly, so the live runners do not compete with the check for time
		channels = append(channels, profileChannel(fmt.Sprintf("ch-%d", i),
			fmt.Sprintf("%s-%d", serviceRefOf(id), i), 3600, flatProfile(230, 0)))
	}
	engine := newFakeHistoryEngine()
	close(engine.release)
	fetcher := &fakeFetcher{readingsGate: make(chan struct{})}
	rt := startOccupancyRuntime(t, testEnvironment(id, channels...), &fakePublisher{}, newFakeHistoryJobs(), engine.run, withWrapper(fetcher))

	done := make(chan error, 1)
	go func() {
		_, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, "")
		done <- err
	}()
	//the queries pile up against the gate until the limit is reached, and no
	//further one may start before one of them returns
	if !waitFor(10*time.Second, func() bool { return fetcher.peakOccupancyQueries() >= historyOccupiedQueries }) {
		t.Errorf("expected %d queries in flight, saw %d", historyOccupiedQueries, fetcher.peakOccupancyQueries())
	}
	if peak := fetcher.peakOccupancyQueries(); peak > historyOccupiedQueries {
		t.Errorf("%d queries were in flight at once, the limit is %d", peak, historyOccupiedQueries)
	}
	close(fetcher.readingsGate)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the run was refused although no channel holds readings: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the check never finished")
	}
	if peak := fetcher.peakOccupancyQueries(); peak > historyOccupiedQueries {
		t.Errorf("%d queries were in flight at once, the limit is %d", peak, historyOccupiedQueries)
	}
	if got := len(fetcher.occupancyChecks()); got != len(channels) {
		t.Errorf("expected one query per channel, %d against %d", got, len(channels))
	}
	waitForHistory(t, rt, id)
}

// TestAResumedHistoryRunDoesNotAskAboutItsWindow: a resume continues a run that
// has written into its window on purpose, and refusing it would leave the
// environment standing at the instant the interrupted run had reached.
func TestAResumedHistoryRunDoesNotAskAboutItsWindow(t *testing.T) {
	const id = "env-hist-resume-occupied"
	def := occupancyEnvironment(id)
	from := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)
	jobs := newFakeHistoryJobs()
	//a run of a previous incarnation, still running: the start resumes it
	if err := jobs.Save(context.Background(), historyRecordOf(HistoryStatus{
		EnvironmentId: id, State: HistoryRunning, From: from, To: time.Now(), StartedAt: from,
	}, def)); err != nil {
		t.Fatal(err)
	}
	engine := newFakeHistoryEngine()
	close(engine.release)
	//every channel of the environment holds readings, which is what a resumed
	//run must not be refused over
	fetcher := &fakeFetcher{occupied: map[string]bool{
		serviceRefOf(id) + "-a1": true,
		serviceRefOf(id) + "-a2": true,
		serviceRefOf(id) + "-b1": true,
	}}
	rt := startOccupancyRuntime(t, def, occupancyPublisher(id), jobs, engine.run, withWrapper(fetcher))

	<-engine.entered
	if checks := fetcher.occupancyChecks(); len(checks) != 0 {
		t.Errorf("the resume asked the wrapper about its own window: %#v", checks)
	}
	if engine.callCount() != 1 {
		t.Errorf("expected the stored run to be resumed once, got %d", engine.callCount())
	}
	waitForHistory(t, rt, id)
}

// TestTheOccupancyCheckReadsWithTheCallersToken: the devices of an environment
// are created with the token of whoever provisioned them, so an admin's device
// under somebody else's environment is not readable with the owner's token - the
// wrapper answers 404 for it, which would leave that channel unchecked.
func TestTheOccupancyCheckReadsWithTheCallersToken(t *testing.T) {
	const id = "env-hist-caller-token"
	engine := newFakeHistoryEngine()
	fetcher := &fakeFetcher{}
	rt := startOccupancyRuntime(t, occupancyEnvironment(id), occupancyPublisher(id), newFakeHistoryJobs(), engine.run, withWrapper(fetcher))

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, "Bearer caller"); err != nil {
		t.Fatalf("a run over a free window has to start, got %v", err)
	}
	<-engine.entered
	close(engine.release)
	waitForHistory(t, rt, id)

	checks := fetcher.occupancyChecks()
	if len(checks) != 3 {
		t.Fatalf("expected one query per publishable channel, got %d", len(checks))
	}
	for _, check := range checks {
		//the caller's token, not the owner's exchanged one the wiring offers
		if check.token != "Bearer caller" {
			t.Errorf("%v was asked with %q rather than the caller's token", check.serviceId, check.token)
		}
	}
}

// TestAWrapperRefusalLeavesTheChannelUnchecked: a 4xx is a state of the platform
// and not an outage - a device this token cannot read, a column the validator
// rejects, or a table the tableworker has not created for a device that was made
// seconds ago. Refusing over one would block every run of that environment for
// good, so the channel is left unchecked and said so.
func TestAWrapperRefusalLeavesTheChannelUnchecked(t *testing.T) {
	for name, status := range map[string]int{
		"a device the token cannot read":       http.StatusNotFound,
		"a column or table the wrapper denies": http.StatusBadRequest,
	} {
		t.Run(name, func(t *testing.T) {
			id := fmt.Sprintf("env-hist-4xx-%d", status)
			log := &recordingWriter{}
			previous := util.Logger
			util.Logger = slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelInfo}))
			t.Cleanup(func() { util.Logger = previous })

			engine := newFakeHistoryEngine()
			close(engine.release)
			refused := &timeseries.StatusError{Status: status, Message: "not for you"}
			fetcher := &fakeFetcher{readingsErrOf: map[string]error{serviceRefOf(id) + "-a1": refused}}
			rt := startOccupancyRuntime(t, occupancyEnvironment(id), occupancyPublisher(id), newFakeHistoryJobs(), engine.run, withWrapper(fetcher))

			if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
				t.Fatalf("a %d has to leave the channel unchecked rather than refuse the run, got %v", status, err)
			}
			<-engine.entered
			waitForHistory(t, rt, id)

			//the other two channels were still asked, so one denied device does
			//not turn the whole check off
			if checks := fetcher.occupancyChecks(); len(checks) != 3 {
				t.Errorf("expected one query per publishable channel, got %d", len(checks))
			}
			if lines := log.count("the timescale-wrapper refused the history window query"); lines != 1 {
				t.Errorf("expected one line per unchecked channel, got %d", lines)
			}
			for _, fragment := range []string{
				"device=" + deviceRefOf(id),
				"service=" + serviceRefOf(id) + "-a1",
				fmt.Sprintf("status=%d", status),
			} {
				if log.count(fragment) == 0 {
					t.Errorf("the warning has to name %q", fragment)
				}
			}
		})
	}
}

// TestAWrapperOutageRefusesTheHistoryRun: a 5xx says nothing about the window,
// and the check matters exactly where something was written before - so the run
// is refused rather than started unchecked, unlike the 4xx above.
func TestAWrapperOutageRefusesTheHistoryRun(t *testing.T) {
	const id = "env-hist-5xx"
	outage := &timeseries.StatusError{Status: http.StatusBadGateway, Message: "no upstream"}
	engine := newFakeHistoryEngine()
	close(engine.release)
	fetcher := &fakeFetcher{readingsErr: outage}
	jobs := newFakeHistoryJobs()
	rt := startOccupancyRuntime(t, occupancyEnvironment(id), occupancyPublisher(id), jobs, engine.run, withWrapper(fetcher))

	_, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, "")
	if !errors.Is(err, outage) {
		t.Fatalf("expected the wrapper's 502 to refuse the run, got %v", err)
	}
	if errors.Is(err, ErrHistoryCheckTimeout) {
		t.Errorf("an outage was reported as a spent budget: %v", err)
	}
	if engine.callCount() != 0 {
		t.Error("the run was started although the window could not be checked")
	}
	if stored := jobs.savesOf(id); len(stored) != 0 {
		t.Errorf("the refused run was written to the store: %#v", stored)
	}
}

// TestAHistoryRunIsRefusedWhenTheCheckRunsOutOfItsBudget: a caller waits on the
// POST while the check runs, so the whole fan-out has to answer inside the
// budget - and a budget that ran out is not "the window is free". The answer
// says what to do about it, and the api turns it into a 503.
func TestAHistoryRunIsRefusedWhenTheCheckRunsOutOfItsBudget(t *testing.T) {
	const id = "env-hist-check-budget"
	//the check has to answer inside the write timeout of the api server, or a
	//slow timescale leaves the client without any answer at all
	if historyOccupiedTimeout >= 10*time.Second {
		t.Errorf("the check budget is %v, which does not fit in the api server's write timeout of 10s", historyOccupiedTimeout)
	}
	engine := newFakeHistoryEngine()
	close(engine.release)
	//never closed: every query hangs until the budget of the check ends it
	fetcher := &fakeFetcher{readingsGate: make(chan struct{})}
	rt := startOccupancyRuntime(t, occupancyEnvironment(id), occupancyPublisher(id), newFakeHistoryJobs(), engine.run, withWrapper(fetcher))

	rt.mux.RLock()
	env := rt.envs[id]
	rt.mux.RUnlock()
	if env == nil {
		t.Fatal("the environment is not running")
	}
	//the check on a budget of its own rather than the five seconds of a request:
	//what is asserted is the answer to a spent budget, not how long it waits
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := rt.checkHistoryOccupied(ctx, env.gen, time.Now().Add(-time.Hour), "")
	if !errors.Is(err, ErrHistoryCheckTimeout) {
		t.Fatalf("expected a spent budget to refuse the run, got %v", err)
	}
	for _, fragment := range []string{"in time", "force: true"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("the refusal has to say %q, it reads %q", fragment, err.Error())
		}
	}
	//neither a window problem nor an occupied one, so the api answers 503
	rangeError := &HistoryRangeError{}
	occupied := &HistoryOccupiedError{}
	if errors.As(err, &rangeError) || errors.As(err, &occupied) {
		t.Errorf("a spent budget must not be reported as a window problem: %v", err)
	}
	if engine.callCount() != 0 {
		t.Error("the check that ran out of budget still started a run")
	}
}

// TestAStoppedRuntimeRefusesAHistoryRunWithoutAskingTheTimescale: during a
// Recreate rollout a POST reaches the instance that is going away. Asking the
// wrapper first would spend the whole budget and then answer 409 for an
// environment this instance cannot run at all.
func TestAStoppedRuntimeRefusesAHistoryRunWithoutAskingTheTimescale(t *testing.T) {
	const id = "env-hist-stopped-check"
	engine := newFakeHistoryEngine()
	close(engine.release)
	fetcher := &fakeFetcher{occupied: map[string]bool{serviceRefOf(id) + "-a1": true}}
	rt := startOccupancyRuntime(t, occupancyEnvironment(id), occupancyPublisher(id), newFakeHistoryJobs(), engine.run, withWrapper(fetcher))
	rt.Stop()

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); !errors.Is(err, repo.ErrNotRunning) {
		t.Fatalf("expected a stopped runtime to refuse the run, got %v", err)
	}
	if checks := fetcher.occupancyChecks(); len(checks) != 0 {
		t.Errorf("a stopped runtime asked the wrapper anyway: %#v", checks)
	}
	if engine.callCount() != 0 {
		t.Error("a stopped runtime started a run")
	}
}

// TestAWrapperStatusThatIsNoPlatformStateRefusesTheHistoryRun: only a 400 and a
// 404 say something about this device or this column. A 401, a 403 or a 429 says
// something about the request itself, and reading it as "the window is free"
// would write a second set of rows over a window nobody looked at.
func TestAWrapperStatusThatIsNoPlatformStateRefusesTheHistoryRun(t *testing.T) {
	for name, status := range map[string]int{
		"an expired token":                http.StatusUnauthorized,
		"a token without the permission":  http.StatusForbidden,
		"a wrapper that is rate limiting": http.StatusTooManyRequests,
	} {
		t.Run(name, func(t *testing.T) {
			id := fmt.Sprintf("env-hist-status-%d", status)
			refused := &timeseries.StatusError{Status: status, Message: "not now"}
			engine := newFakeHistoryEngine()
			close(engine.release)
			jobs := newFakeHistoryJobs()
			fetcher := &fakeFetcher{readingsErrOf: map[string]error{serviceRefOf(id) + "-a1": refused}}
			rt := startOccupancyRuntime(t, occupancyEnvironment(id), occupancyPublisher(id), jobs, engine.run, withWrapper(fetcher))

			_, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, "")
			if !errors.Is(err, refused) {
				t.Fatalf("expected the %d to refuse the run, got %v", status, err)
			}
			if errors.Is(err, ErrHistoryCheckTimeout) {
				t.Errorf("a %d was reported as a spent budget: %v", status, err)
			}
			if engine.callCount() != 0 {
				t.Error("the run was started although the window could not be checked")
			}
			if _, err = rt.HistoryStatusOf(id); !errors.Is(err, ErrNoHistory) {
				t.Errorf("the refused run was left in the registry: %v", err)
			}
			if stored := jobs.savesOf(id); len(stored) != 0 {
				t.Errorf("the refused run was written to the store: %#v", stored)
			}
		})
	}
}

// TestADeniedChannelIsAskedAgainWithTheOwnersToken: the devices of an
// environment are created with the token of whoever provisioned them, so neither
// the caller's token nor the owner's reads all of them. A 404 on the caller's is
// therefore not an answer about the window yet.
func TestADeniedChannelIsAskedAgainWithTheOwnersToken(t *testing.T) {
	const ownerToken = "Bearer token-for-test-owner"
	denied := &timeseries.StatusError{Status: http.StatusNotFound, Message: "no such device"}

	t.Run("the owner sees the readings the caller may not", func(t *testing.T) {
		const id = "env-hist-retry-occupied"
		engine := newFakeHistoryEngine()
		close(engine.release)
		fetcher := &fakeFetcher{readingsOf: func(token string, serviceId string) (bool, error) {
			if serviceId != serviceRefOf(id)+"-a1" {
				return false, nil
			}
			if token == ownerToken {
				return true, nil
			}
			return false, denied
		}}
		jobs := newFakeHistoryJobs()
		rt := startOccupancyRuntime(t, occupancyEnvironment(id), occupancyPublisher(id), jobs, engine.run, withWrapper(fetcher))

		_, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, "Bearer caller")
		occupied := &HistoryOccupiedError{}
		if !errors.As(err, &occupied) {
			t.Fatalf("expected the retry on the owner's token to find the readings, got %v", err)
		}
		if len(occupied.Devices) != 1 || occupied.Devices[0].DeviceId != deviceRefOf(id) {
			t.Fatalf("expected the machine to be named once, got %#v", occupied.Devices)
		}
		//three channels on the caller's token and exactly the denied one again
		checks := fetcher.occupancyChecks()
		if len(checks) != 4 {
			t.Fatalf("expected three queries and one retry, got %d: %#v", len(checks), checks)
		}
		retries := 0
		for _, check := range checks {
			if check.token != ownerToken {
				continue
			}
			retries++
			if check.serviceId != serviceRefOf(id)+"-a1" {
				t.Errorf("a channel that was not denied was asked again: %v", check.serviceId)
			}
		}
		if retries != 1 {
			t.Errorf("expected exactly one retry on the owner's token, got %d", retries)
		}
		if engine.callCount() != 0 {
			t.Error("the refused run still started the engine")
		}
		if stored := jobs.savesOf(id); len(stored) != 0 {
			t.Errorf("the refused run was written to the store: %#v", stored)
		}
	})

	t.Run("neither token reads the device", func(t *testing.T) {
		const id = "env-hist-retry-denied"
		log := &recordingWriter{}
		previous := util.Logger
		util.Logger = slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelInfo}))
		t.Cleanup(func() { util.Logger = previous })

		engine := newFakeHistoryEngine()
		fetcher := &fakeFetcher{readingsOf: func(token string, serviceId string) (bool, error) {
			if serviceId == serviceRefOf(id)+"-a1" {
				return false, denied
			}
			return false, nil
		}}
		rt := startOccupancyRuntime(t, occupancyEnvironment(id), occupancyPublisher(id), newFakeHistoryJobs(), engine.run, withWrapper(fetcher))

		if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, "Bearer caller"); err != nil {
			t.Fatalf("a second 404 has to leave the channel unchecked rather than refuse the run, got %v", err)
		}
		<-engine.entered
		close(engine.release)
		waitForHistory(t, rt, id)

		if checks := fetcher.occupancyChecks(); len(checks) != 4 {
			t.Errorf("expected three queries and one retry, got %d", len(checks))
		}
		//one line, not one per attempt: the channel is unchecked once
		if lines := log.count("the timescale-wrapper refused the history window query"); lines != 1 {
			t.Errorf("expected one line per unchecked channel, got %d", lines)
		}
	})
}

// TestATimeShapeThatCannotBeReadRefusesTheHistoryRun: a device repository that
// is unreachable answers the same way for every channel, so a run started over
// it would check nothing at all and report a window as free that nobody looked
// at. A service that carries no time path is the other case and is skipped.
func TestATimeShapeThatCannotBeReadRefusesTheHistoryRun(t *testing.T) {
	const id = "env-hist-shape-broken"
	unreachable := errors.New("the device repository does not answer")
	engine := newFakeHistoryEngine()
	close(engine.release)
	publisher := &fakePublisher{shapeErr: map[string]error{
		serviceRefOf(id) + "-a1":     unreachable,
		serviceRefOf(id) + "-silent": devices.ErrNoTimePath,
	}}
	jobs := newFakeHistoryJobs()
	fetcher := &fakeFetcher{}
	rt := startOccupancyRuntime(t, occupancyEnvironment(id), publisher, jobs, engine.run, withWrapper(fetcher))

	_, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, "")
	if !errors.Is(err, unreachable) {
		t.Fatalf("expected the lookup failure to refuse the run, got %v", err)
	}
	rangeError := &HistoryRangeError{}
	occupied := &HistoryOccupiedError{}
	if errors.As(err, &rangeError) || errors.As(err, &occupied) {
		t.Errorf("a lookup that failed must not be reported as a window problem: %v", err)
	}
	//refused before the fan-out, so no channel of the environment was asked
	if checks := fetcher.occupancyChecks(); len(checks) != 0 {
		t.Errorf("the wrapper was asked although the shapes could not be read: %#v", checks)
	}
	if engine.callCount() != 0 {
		t.Error("the run was started although the window could not be checked")
	}
	if _, err = rt.HistoryStatusOf(id); !errors.Is(err, ErrNoHistory) {
		t.Errorf("the refused run was left in the registry: %v", err)
	}
	if stored := jobs.savesOf(id); len(stored) != 0 {
		t.Errorf("the refused run was written to the store: %#v", stored)
	}

	rt.mux.RLock()
	env := rt.envs[id]
	rt.mux.RUnlock()
	if env == nil {
		t.Fatal("the environment is gone")
	}
	env.mux.Lock()
	underHistory := env.underHistory
	env.mux.Unlock()
	if underHistory {
		t.Error("the environment was taken away for a run that was refused")
	}
}

// TestAServiceThatCannotCarryATimestampIsSkippedByTheCheck: a declaration
// problem of the device type is a property of the service, like a missing time
// path, so the channel only computes and the run starts; only a lookup that
// failed refuses.
func TestAServiceThatCannotCarryATimestampIsSkippedByTheCheck(t *testing.T) {
	const id = "env-hist-shape-unusable"
	engine := newFakeHistoryEngine()
	close(engine.release)
	publisher := &fakePublisher{shapeErr: map[string]error{
		serviceRefOf(id) + "-a1":     fmt.Errorf("%w: the value variable is declared as a string", devices.ErrUnusableTimeShape),
		serviceRefOf(id) + "-silent": devices.ErrNoTimePath,
	}}
	jobs := newFakeHistoryJobs()
	fetcher := &fakeFetcher{}
	rt := startOccupancyRuntime(t, occupancyEnvironment(id), publisher, jobs, engine.run, withWrapper(fetcher))

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("expected the run to start with the unusable channel skipped, got %v", err)
	}
	for _, check := range fetcher.occupancyChecks() {
		if check.serviceId == serviceRefOf(id)+"-a1" || check.serviceId == serviceRefOf(id)+"-silent" {
			t.Errorf("a channel that can never publish was asked about its window: %#v", check)
		}
	}
	if len(fetcher.occupancyChecks()) == 0 {
		t.Error("the publishable channels were not asked at all")
	}
	if !waitFor(time.Second, func() bool { return engine.callCount() == 1 }) {
		t.Error("the run did not start")
	}
}

// TestAnEnvironmentThatIsAlreadyBusyIsRefusedBeforeTheTimescaleIsAsked: the
// check costs one query per channel and a caller waits on the POST for it, so a
// second run or a run next to a backfill is named out of the registries first.
// registerHistory decides it again under the lifecycle mutex.
func TestAnEnvironmentThatIsAlreadyBusyIsRefusedBeforeTheTimescaleIsAsked(t *testing.T) {
	t.Run("a run of this environment", func(t *testing.T) {
		const id = "env-hist-busy-run"
		engine := newFakeHistoryEngine()
		fetcher := &fakeFetcher{}
		rt := startOccupancyRuntime(t, occupancyEnvironment(id), occupancyPublisher(id), newFakeHistoryJobs(), engine.run, withWrapper(fetcher))

		if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
			t.Fatalf("unable to start the first run: %v", err)
		}
		<-engine.entered
		asked := len(fetcher.occupancyChecks())

		if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); !errors.Is(err, ErrHistoryRunning) {
			t.Errorf("expected a second run to be refused, got %v", err)
		}
		if got := len(fetcher.occupancyChecks()); got != asked {
			t.Errorf("the refused second run spent %d further queries", got-asked)
		}
		close(engine.release)
		waitForHistory(t, rt, id)
	})

	t.Run("a backfill of this environment", func(t *testing.T) {
		const id = "env-hist-busy-backfill"
		engine := newFakeHistoryEngine()
		close(engine.release)
		publisher := occupancyPublisher(id)
		publisher.gate = make(chan struct{})
		fetcher := &fakeFetcher{}
		rt := startOccupancyRuntime(t, occupancyEnvironment(id), publisher, newFakeHistoryJobs(), engine.run, withWrapper(fetcher))

		if _, err := rt.StartBackfill(id, backfillFrom, backfillTo); err != nil {
			t.Fatalf("unable to start the backfill: %v", err)
		}
		if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); !errors.Is(err, ErrBackfillRunning) {
			t.Errorf("expected the run to be refused while a backfill is running, got %v", err)
		}
		if checks := fetcher.occupancyChecks(); len(checks) != 0 {
			t.Errorf("the refused run asked the wrapper anyway: %#v", checks)
		}
		if engine.callCount() != 0 {
			t.Error("the refused run still started the engine")
		}
		close(publisher.gate)
		waitForBackfill(t, rt, id)
	})
}
