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
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// fakeHistoryEngine stands in for the simulation so that the lifecycle around it
// can be driven exactly: it says when it is inside, waits to be let out, and can
// end in any of the four ways a real run can.
type fakeHistoryEngine struct {
	entered   chan struct{}
	left      chan struct{}
	once      sync.Once
	leftOnce  sync.Once
	release   chan struct{}
	work      func(env *environment)
	result    HistoryResult
	err       error
	panicWith string

	mux    sync.Mutex
	calls  int
	window [2]time.Time
	// resumed and checkpoint are what the lifecycle handed the engine: the
	// checkpoint a resume continues from, and the seam a run writes through.
	resumed    *repo.HistoryCheckpoint
	checkpoint historyCheckpointFunc

	// boundary, when set, is what the fake reports through that seam as soon as
	// it is inside, before it waits to be let out.
	boundary *repo.HistoryJobProgress
}

func newFakeHistoryEngine() *fakeHistoryEngine {
	return &fakeHistoryEngine{
		entered: make(chan struct{}), left: make(chan struct{}), release: make(chan struct{}),
	}
}

func (this *fakeHistoryEngine) run(ctx context.Context, env *environment, gen *generation, from time.Time, to time.Time, chase bool, progress historyProgress, resume *repo.HistoryCheckpoint, checkpoint historyCheckpointFunc) (HistoryResult, error) {
	this.mux.Lock()
	this.calls++
	this.window = [2]time.Time{from, to}
	this.resumed = resume
	this.checkpoint = checkpoint
	this.mux.Unlock()
	this.once.Do(func() { close(this.entered) })
	defer this.leftOnce.Do(func() { close(this.left) })
	if this.boundary != nil && checkpoint != nil {
		//a stored checkpoint without simulating a window for it, which is what a
		//lifecycle test needs to resume from
		_ = checkpoint(*this.boundary)
	}
	select {
	case <-this.release:
	case <-ctx.Done():
	}
	//the real engine checks the context before every event, so a run that was
	//ended reports that itself rather than leaving it to be read afterwards
	if err := ctx.Err(); err != nil {
		return HistoryResult{}, err
	}
	if this.work != nil {
		this.work(env)
	}
	if this.panicWith != "" {
		panic(this.panicWith)
	}
	return this.result, this.err
}

func (this *fakeHistoryEngine) callCount() int {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.calls
}

// writeAssetValue is the usual work of a fake run: it leaves a value in the
// state, which is what the environment is supposed to keep afterwards.
func writeAssetValue(key string, value float64) func(env *environment) {
	return func(env *environment) {
		env.mux.Lock()
		defer env.mux.Unlock()
		env.assetStates(testAssetId)[key] = value
		env.dirty = true
	}
}

func startRuntimeWithEngine(t *testing.T, cfg config.Config, envs *fakeEnvironments, states *fakeStates, publisher *fakePublisher, engine historyEngineFunc) *Runtime {
	t.Helper()
	return startRuntimeWithJobs(t, cfg, envs, states, newFakeHistoryJobs(), publisher, engine)
}

// startRuntimeWithJobs is the same with a job store the test keeps: a run stored
// as running is resumed by the Start below, which is what several tests here are
// about.
func startRuntimeWithJobs(t *testing.T, cfg config.Config, envs *fakeEnvironments, states *fakeStates, jobs repo.HistoryJobs, publisher *fakePublisher, engine historyEngineFunc) *Runtime {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rt := newRuntime(cfg, envs, states, nil, jobs, publisher)
	if engine != nil {
		rt.historyEngine = engine
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("unable to start the runtime: %v", err)
	}
	t.Cleanup(rt.Stop)
	return rt
}

// waitForHistory waits until the run of one environment has stopped running.
func waitForHistory(t *testing.T, rt *Runtime, id string) HistoryStatus {
	t.Helper()
	var status HistoryStatus
	done := waitFor(15*time.Second, func() bool {
		var err error
		status, err = rt.HistoryStatusOf(id)
		return err == nil && status.State != HistoryRunning
	})
	if !done {
		t.Fatalf("the history run of %v did not finish, it is %#v", id, status)
	}
	return status
}

// waitForHistoryHandover waits until the run of one environment has given the
// environment back. It is the wait for a suspended run, which keeps its status:
// the store says running and the next start continues it, so waiting for the
// status to turn would wait for ever.
func waitForHistoryHandover(t *testing.T, rt *Runtime, id string) {
	t.Helper()
	given := waitFor(15*time.Second, func() bool {
		rt.mux.RLock()
		env := rt.envs[id]
		rt.mux.RUnlock()
		if env == nil {
			return true
		}
		env.mux.Lock()
		defer env.mux.Unlock()
		return !env.underHistory
	})
	if !given {
		t.Fatalf("the history run of %v never handed its environment back", id)
	}
}

// liveEventsAfter returns the live readings published after one instant, which
// is how a test tells the simulation that follows a run from the one that
// preceded it.
func liveEventsAfter(publisher *fakePublisher, after time.Time) []publishedEvent {
	result := []publishedEvent{}
	for _, event := range publisher.all() {
		if event.live && event.at.After(after) {
			result = append(result, event)
		}
	}
	return result
}

func historyTestEnvironment(id string) domain.Environment {
	//an hourly channel, so the live runners never tick during a test that is
	//about the lifecycle rather than about the readings
	return testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 3600, flatProfile(230, 0)))
}

func genOf(rt *Runtime, id string) *generation {
	rt.mux.RLock()
	defer rt.mux.RUnlock()
	env := rt.envs[id]
	if env == nil {
		return nil
	}
	return env.gen
}

// TestAHistoryRunReplacesTheStateAndFlushesItBeforeTheRunnersStart: the state the
// run arrived at is the point of the mode, and it is on disk before a live tick
// can move it - a crash right after the handover must not lose the window.
func TestAHistoryRunReplacesTheStateAndFlushesItBeforeTheRunnersStart(t *testing.T) {
	const id = "env-hist-replace"
	engine := newFakeHistoryEngine()
	engine.work = writeAssetValue("meter", 4711)
	engine.result = HistoryResult{Published: 12}
	states := newFakeStates()
	//an hour long flush interval: the only write that can happen is the one the
	//handover makes
	rt := startRuntimeWithEngine(t, testConfig(time.Hour), newFakeEnvironments(historyTestEnvironment(id)), states, &fakePublisher{}, engine.run)

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	<-engine.entered
	close(engine.release)
	status := waitForHistory(t, rt, id)

	if status.State != HistoryDone {
		t.Errorf("expected the run to be done, it is %v (%v)", status.State, status.Error)
	}
	if status.Published != 12 || status.FinishedAt == nil {
		t.Errorf("the result did not reach the status: %#v", status)
	}

	saved := states.savedFor(id)
	if len(saved) == 0 {
		t.Fatal("the state the run arrived at was never written")
	}
	last := saved[len(saved)-1].state
	if value, _ := asFloat(last.Assets[testAssetId]["meter"]); value != 4711 {
		t.Errorf("the written state holds %v, expected the 4711 the run produced", last.Assets[testAssetId]["meter"])
	}

	rt.mux.RLock()
	env := rt.envs[id]
	rt.mux.RUnlock()
	env.mux.Lock()
	underHistory := env.underHistory
	inMemory, _ := asFloat(env.assetStates(testAssetId)["meter"])
	env.mux.Unlock()
	if underHistory {
		t.Error("the environment is still marked as owned by the run")
	}
	if inMemory != 4711 {
		t.Errorf("the live environment holds %v, expected the state the run arrived at", inMemory)
	}
}

// TestTheRunIsDoneOnlyOnceTheSimulationRunsAgain is what "done" is worth: a
// caller that polls until done and then reads the state has to find a running
// environment, not one between two lifecycles.
func TestTheRunIsDoneOnlyOnceTheSimulationRunsAgain(t *testing.T) {
	const id = "env-hist-done"
	engine := newFakeHistoryEngine()
	close(engine.release)
	rt := startRuntimeWithEngine(t, testConfig(time.Hour), newFakeEnvironments(historyTestEnvironment(id)), newFakeStates(), &fakePublisher{}, engine.run)

	before := genOf(rt, id)
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	waitForHistory(t, rt, id)

	//read in the same breath as the status: the handover holds the lifecycle
	//mutex over both, so a generation that is still the old one here means the
	//status turned before the runners were started
	after := genOf(rt, id)
	if after == nil {
		t.Fatal("the environment is gone after the run")
	}
	if after == before {
		t.Error("the status says done while the environment still runs the generation of the run")
	}
}

// TestTheEnvironmentIsHandedBackAfterEveryOutcome: an environment left marked as
// owned by a run would refuse every state change and never tick again, so the
// handover has to happen after a failure and an abort as much as after a success.
func TestTheEnvironmentIsHandedBackAfterEveryOutcome(t *testing.T) {
	for name, testCase := range map[string]struct {
		prepare func(engine *fakeHistoryEngine)
		cancel  bool
		want    HistoryState
		message string
	}{
		"a run that finished":  {func(engine *fakeHistoryEngine) {}, false, HistoryDone, ""},
		"a run that failed":    {func(engine *fakeHistoryEngine) { engine.err = errors.New("the simulation broke") }, false, HistoryFailed, "broke"},
		"a run that panicked":  {func(engine *fakeHistoryEngine) { engine.panicWith = "a simulation bug" }, false, HistoryFailed, "a simulation bug"},
		"a run that was ended": {func(engine *fakeHistoryEngine) {}, true, HistoryCancelled, ""},
	} {
		t.Run(name, func(t *testing.T) {
			id := "env-hist-outcome"
			engine := newFakeHistoryEngine()
			engine.work = writeAssetValue("meter", 815)
			testCase.prepare(engine)
			rt := startRuntimeWithEngine(t, testConfig(time.Hour), newFakeEnvironments(historyTestEnvironment(id)), newFakeStates(), &fakePublisher{}, engine.run)

			before := genOf(rt, id)
			if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
				t.Fatalf("unable to start the history run: %v", err)
			}
			<-engine.entered
			if testCase.cancel {
				if _, err := rt.CancelHistory(id); err != nil {
					t.Fatalf("unable to cancel the run: %v", err)
				}
			}
			close(engine.release)
			status := waitForHistory(t, rt, id)

			if status.State != testCase.want {
				t.Errorf("expected %v, got %v (%v)", testCase.want, status.State, status.Error)
			}
			if testCase.message != "" && !strings.Contains(status.Error, testCase.message) {
				t.Errorf("expected the error to mention %q, got %q", testCase.message, status.Error)
			}

			rt.mux.RLock()
			env := rt.envs[id]
			rt.mux.RUnlock()
			if env == nil {
				t.Fatal("the environment was dropped")
			}
			env.mux.Lock()
			underHistory := env.underHistory
			meter, _ := asFloat(env.assetStates(testAssetId)["meter"])
			env.mux.Unlock()
			if underHistory {
				t.Error("the environment is still marked as owned by the run")
			}
			//the channels tick again whatever became of the run: an environment
			//whose runners are not restarted is silent until the next edit
			if after := genOf(rt, id); after == nil || after == before {
				t.Error("the runners were not started again after the run")
			}
			//the partial state stands: there is no rollback, and a run that broke
			//halfway leaves a consistent state of an earlier instant
			if !testCase.cancel && engine.panicWith == "" && meter != 815 {
				t.Errorf("the state the run produced is gone, the meter reads %v", meter)
			}
			//and the environment takes state changes again
			if err := rt.SetState(id, repo.StateChange{Assets: map[string]map[string]interface{}{testAssetId: {"x": 1.0}}}); err != nil {
				t.Errorf("the environment still refuses a state change after the run: %v", err)
			}
		})
	}
}

// TestAReloadDuringAHistoryRunIsSkippedAndTakesEffectAtItsEnd: restarting the
// channels mid-run would tear the virtual clock out of it, and dropping the edit
// would lose it, so it is applied by the handover, which reads the definition
// again.
func TestAReloadDuringAHistoryRunIsSkippedAndTakesEffectAtItsEnd(t *testing.T) {
	const id = "env-hist-reload"
	engine := newFakeHistoryEngine()
	envs := newFakeEnvironments(historyTestEnvironment(id))
	rt := startRuntimeWithEngine(t, testConfig(time.Hour), envs, newFakeStates(), &fakePublisher{}, engine.run)

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	<-engine.entered
	running := genOf(rt, id)

	edited := historyTestEnvironment(id)
	edited.Zones[0].Assets[0].Channels = append(edited.Zones[0].Assets[0].Channels,
		profileChannel("ch-2", serviceRefOf(id)+"-added", 1, flatProfile(42, 0)))
	if _, err := envs.Put(t.Context(), edited); err != nil {
		t.Fatal(err)
	}
	rt.Reload(id)

	if again := genOf(rt, id); again != running {
		t.Error("the reload restarted the channels the run had stopped")
	}
	close(engine.release)
	waitForHistory(t, rt, id)

	after := genOf(rt, id)
	if after == nil || len(after.sensors) != 2 {
		t.Fatalf("expected the edited definition to be in effect after the run, got %d channels", len(after.sensors))
	}
}

// TestRemovingAnEnvironmentEndsItsHistoryRun: the run publishes to platform
// devices that are being deleted with the environment, and it must not be
// restarted afterwards.
func TestRemovingAnEnvironmentEndsItsHistoryRun(t *testing.T) {
	const id = "env-hist-remove"
	engine := newFakeHistoryEngine()
	envs := newFakeEnvironments(historyTestEnvironment(id))
	rt := startRuntimeWithEngine(t, testConfig(time.Hour), envs, newFakeStates(), &fakePublisher{}, engine.run)

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	<-engine.entered
	if err := envs.Delete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	rt.Remove(id)
	close(engine.release)

	status := waitForHistory(t, rt, id)
	if status.State != HistoryCancelled {
		t.Errorf("expected the run to be cancelled, it is %v", status.State)
	}
	if gen := genOf(rt, id); gen != nil {
		t.Error("the removed environment was started again by the handover")
	}
}

// TestStoppingTheRuntimeEndsAHistoryRunWithoutDeadlocking is the trap the two
// phases exist for: the handover needs the lifecycle mutex Stop holds, so a Stop
// that waited for it would never return.
func TestStoppingTheRuntimeEndsAHistoryRunWithoutDeadlocking(t *testing.T) {
	const id = "env-hist-stop"
	engine := newFakeHistoryEngine()
	states := newFakeStates()
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(historyTestEnvironment(id)), states, nil, newFakeHistoryJobs(), &fakePublisher{})
	rt.historyEngine = engine.run
	if err := rt.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	<-engine.entered
	engine.work = writeAssetValue("meter", 99)

	stopped := make(chan struct{})
	go func() {
		rt.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("Stop did not return while a history run was in flight")
	}

	waitForHistoryHandover(t, rt, id)
	//a shutdown suspends the run rather than ending it, so its status stays
	//running - the store says the same, and the next start continues it
	status, err := rt.HistoryStatusOf(id)
	if err != nil || status.State != HistoryRunning || status.FinishedAt != nil {
		t.Errorf("expected the suspended run to stay running, got %#v (%v)", status, err)
	}
	//the partial state is not lost: Stop flushes what the engine phase left
	//behind before the handover gets the lifecycle mutex
	if len(states.savedFor(id)) == 0 {
		t.Error("the state the run had reached was never written")
	}
	//and no further run is accepted
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); !errors.Is(err, repo.ErrNotRunning) {
		t.Errorf("expected a stopped runtime to refuse a run, got %v", err)
	}
}

// TestAHistoryRunLocksOutEverythingThatWouldMixInThePresent is the whole
// interaction matrix: while an environment stands at a past instant, every way
// of touching it from outside has to be refused rather than half applied.
func TestAHistoryRunLocksOutEverythingThatWouldMixInThePresent(t *testing.T) {
	const id = "env-hist-exclusive"
	engine := newFakeHistoryEngine()
	publisher := &fakePublisher{}
	rt := startRuntimeWithEngine(t, testConfig(time.Hour), newFakeEnvironments(historyTestEnvironment(id)), newFakeStates(), publisher, engine.run)

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	<-engine.entered

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); !errors.Is(err, ErrHistoryRunning) {
		t.Errorf("expected a second run to be refused, got %v", err)
	}
	if _, err := rt.StartBackfill(id, backfillFrom, backfillTo); !errors.Is(err, ErrHistoryRunning) {
		t.Errorf("expected a backfill to be refused while a run owns the environment, got %v", err)
	}
	change := repo.StateChange{Assets: map[string]map[string]interface{}{testAssetId: {"x": 1.0}}}
	if err := rt.SetState(id, change); !errors.Is(err, ErrHistoryRunning) {
		t.Errorf("expected a state change to be refused, got %v", err)
	}
	if _, err := rt.Snapshot(id); !errors.Is(err, ErrHistoryRunning) {
		t.Errorf("expected a snapshot to be refused, got %v", err)
	}
	//a command is dropped rather than answered: the environment stands in the
	//past, and the responder would carry a reading of a moment that is not now
	before := publisher.count()
	if !rt.HandleCommand(deviceRefOf(id), serviceRefOf(id), nil, func(interface{}) {
		t.Error("a command was answered while the environment stands at a past instant")
	}) {
		t.Error("the runtime disowned a device it holds")
	}
	if publisher.count() != before {
		t.Error("a command published a reading while the environment stands at a past instant")
	}

	close(engine.release)
	waitForHistory(t, rt, id)

	//and everything is allowed again afterwards
	if err := rt.SetState(id, change); err != nil {
		t.Errorf("the environment still refuses a state change after the run: %v", err)
	}
	if _, err := rt.Snapshot(id); err != nil {
		t.Errorf("the environment still refuses a snapshot after the run: %v", err)
	}
}

// TestABackfillAndAHistoryRunExcludeEachOtherBothWays: the job publishes into a
// past window and the run publishes into the same one, from an environment whose
// state it is replacing.
func TestABackfillAndAHistoryRunExcludeEachOtherBothWays(t *testing.T) {
	const id = "env-hist-vs-backfill"
	engine := newFakeHistoryEngine()
	publisher := &fakePublisher{gate: make(chan struct{})}
	env := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 60, hourlyProfile()))
	rt := startRuntimeWithEngine(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher, engine.run)

	if _, err := rt.StartBackfill(id, backfillFrom, backfillTo); err != nil {
		t.Fatalf("unable to start the backfill: %v", err)
	}
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); !errors.Is(err, ErrBackfillRunning) {
		t.Errorf("expected the run to be refused while a backfill is running, got %v", err)
	}
	if engine.callCount() != 0 {
		t.Error("the refused run still started the engine")
	}
	close(publisher.gate)
	waitForBackfill(t, rt, id)

	//and with the job finished the run is allowed
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Errorf("expected a run after the backfill finished, got %v", err)
	}
	<-engine.entered
	close(engine.release)
	waitForHistory(t, rt, id)
}

// TestARefusedHistoryWindowDoesNotTouchTheSimulation: the window and the volume
// are checked before anything is stopped, so a caller that asks for an impossible
// run does not interrupt the environment for it.
func TestARefusedHistoryWindowDoesNotTouchTheSimulation(t *testing.T) {
	const id = "env-hist-refused-window"
	engine := newFakeHistoryEngine()
	publisher := &fakePublisher{}
	//a one second channel, so the live simulation is visibly running
	env := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 1, flatProfile(230, 0)))
	rt := startRuntimeWithEngine(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher, engine.run)

	for name, from := range map[string]time.Time{
		"an instant in the future": time.Now().Add(time.Hour),
		"before the platform":      time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC),
		//a year of one second data is far past the step cap
		"too many steps": time.Now().Add(-360 * 24 * time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := rt.StartHistory(id, from, false, "")
			rangeError := &HistoryRangeError{}
			if !errors.As(err, &rangeError) {
				t.Fatalf("expected a HistoryRangeError, got %v", err)
			}
		})
	}

	if engine.callCount() != 0 {
		t.Error("a refused window still started the engine")
	}
	if _, err := rt.HistoryStatusOf(id); !errors.Is(err, ErrNoHistory) {
		t.Errorf("a refused window left a run in the registry: %v", err)
	}
	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Error("the live simulation stopped over a window that was refused")
	}
}

// TestAnAmbiguousGridRefusesTheRunBeforeAnythingIsStopped: the identities a
// checkpoint keys the ticks by can collide although every channel id of the
// document is unique - a channel called "context:shift" next to a context source
// shift, or one called "ch-1:publish" next to the split channel ch-1. That is a
// document the api stores, so the refusal has to happen where the window is
// checked: after the state has been discarded it would be a run that fails and
// an environment that lost its live state for nothing.
func TestAnAmbiguousGridRefusesTheRunBeforeAnythingIsStopped(t *testing.T) {
	documents := map[string]func(id string) domain.Environment{
		"a channel named like a context source": func(id string) domain.Environment {
			def := testEnvironment(id, profileChannel("context:shift", serviceRefOf(id), 1, flatProfile(230, 0)))
			def.ContextSources = map[string]domain.Source{
				"shift": {Kind: domain.SourceProfile, IntervalSeconds: 1, Profile: profilePointer(flatProfile(1, 0))},
			}
			return def
		},
		"a channel named like the publish half of a split channel": func(id string) domain.Environment {
			//a source interval of its own is what splits a channel into the grid
			//that computes and the grid that publishes
			split := profileChannel("ch-1", serviceRefOf(id), 2, flatProfile(230, 0))
			split.Source.IntervalSeconds = 1
			return testEnvironment(id, split,
				profileChannel("ch-1:publish", serviceRefOf(id), 1, flatProfile(12, 0)))
		},
	}
	ids := 0
	for name, document := range documents {
		ids++
		id := fmt.Sprintf("env-hist-ambiguous-%d", ids)
		t.Run(name, func(t *testing.T) {
			def := document(id)
			publisher := &fakePublisher{}
			engine := newFakeHistoryEngine()
			rt := startRuntimeWithEngine(t, testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), publisher, engine.run)

			//a value of the live simulation, which a run would discard with the
			//rest of the state before it computed anything
			rt.mux.RLock()
			env := rt.envs[id]
			rt.mux.RUnlock()
			if env == nil {
				t.Fatal("the environment is not running")
			}
			env.mux.Lock()
			env.assetStates(testAssetId)["marker"] = 4711.0
			env.mux.Unlock()

			_, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, "")
			rangeError := &HistoryRangeError{}
			if !errors.As(err, &rangeError) {
				t.Fatalf("expected a HistoryRangeError, got %v", err)
			}
			if engine.callCount() != 0 {
				t.Error("the run was started although its grids are ambiguous")
			}
			if _, err = rt.HistoryStatusOf(id); !errors.Is(err, ErrNoHistory) {
				t.Errorf("the refused run was left in the registry: %v", err)
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
		})
	}
}

func TestTheHistoryRunOfAnUnknownEnvironmentIsNotRunning(t *testing.T) {
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), &fakePublisher{})
	if _, err := rt.StartHistory("nobody", time.Now().Add(-time.Hour), false, ""); !errors.Is(err, repo.ErrNotRunning) {
		t.Errorf("expected ErrNotRunning, got %v", err)
	}
	if _, err := rt.HistoryStatusOf("nobody"); !errors.Is(err, ErrNoHistory) {
		t.Errorf("expected ErrNoHistory, got %v", err)
	}
	if _, err := rt.CancelHistory("nobody"); !errors.Is(err, ErrNoHistory) {
		t.Errorf("expected ErrNoHistory, got %v", err)
	}
}

// TestARunThatFinishedIsNotReportedAsCancelled: an abort or a shutdown arriving
// milliseconds after the last step does not make the window incomplete, and a
// caller that reads cancelled has to go and find out what is missing.
func TestARunThatFinishedIsNotReportedAsCancelled(t *testing.T) {
	const id = "env-hist-late-cancel"
	engine := newFakeHistoryEngine()
	engine.result = HistoryResult{Published: 7}
	rt := startRuntimeWithEngine(t, testConfig(time.Hour), newFakeEnvironments(historyTestEnvironment(id)), newFakeStates(), &fakePublisher{}, engine.run)

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	<-engine.entered

	//the handover is held up, so the abort lands in the window between the engine
	//returning and the outcome being written
	rt.lifecycle.Lock()
	close(engine.release)
	<-engine.left
	if _, err := rt.CancelHistory(id); err != nil {
		t.Fatalf("unable to abort the run: %v", err)
	}
	rt.lifecycle.Unlock()

	status := waitForHistory(t, rt, id)
	if status.State != HistoryDone {
		t.Errorf("the run had finished before the abort reached it, but it is %v", status.State)
	}
	if status.Published != 7 {
		t.Errorf("expected the counters of the finished run, got %#v", status)
	}
}

// TestAHistoryRunNamesEveryChannelThatPublishesNothing: a device repository that
// was briefly unreachable makes every channel unpublishable, and a run that then
// reports done with nothing published and no reason is the failure that takes
// longest to understand.
func TestAHistoryRunNamesEveryChannelThatPublishesNothing(t *testing.T) {
	const id = "env-hist-silent"
	channel := profileChannel("ch-1", serviceRefOf(id), 60, domain.ProfileSource{Base: 3600, Cumulative: true})
	publisher := &fakePublisher{shapeErr: map[string]error{serviceRefOf(id): devices.ErrNoTimePath}}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(testEnvironment(id, channel)), newFakeStates(), publisher)

	if _, err := rt.StartHistory(id, time.Now().Add(-2*time.Minute), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	status := waitForHistory(t, rt, id)

	if status.State != HistoryDone || status.Published != 0 {
		t.Fatalf("expected a finished run that published nothing, got %#v", status)
	}
	if len(status.Channels) != 1 {
		t.Fatalf("expected the one channel to be reported, got %#v", status.Channels)
	}
	reported := status.Channels[0]
	if reported.ChannelId != "ch-1" || reported.Publishable {
		t.Errorf("expected ch-1 to be reported as unpublishable, got %#v", reported)
	}
	if !strings.Contains(reported.Reason, devices.ErrNoTimePath.Error()) {
		t.Errorf("expected the reason to name the missing time path, got %q", reported.Reason)
	}
	if reported.Silent == 0 {
		t.Error("the steps the channel computed without sending have to be counted, or it looks like it never ran")
	}
}

// TestAHistoryRunChecksTheVolumeOfTheGenerationItActuallyRuns: the first check
// runs before the lifecycle mutex is taken, so a reload can replace an hourly
// document by a one second one in between - and the second document is the one
// that would be simulated.
func TestAHistoryRunChecksTheVolumeOfTheGenerationItActuallyRuns(t *testing.T) {
	const id = "env-hist-volume-swap"
	engine := newFakeHistoryEngine()
	close(engine.release)
	rt := startRuntimeWithEngine(t, testConfig(time.Hour), newFakeEnvironments(historyTestEnvironment(id)), newFakeStates(), &fakePublisher{}, engine.run)

	//a one second grid over a year, which is what the cap exists for
	dense := newGeneration(testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 1, flatProfile(230, 0))), nil)

	//the handover mutex is held, so the call gets past the check on the hourly
	//generation and then waits where a reload would have overtaken it
	rt.lifecycle.Lock()
	answered := make(chan error, 1)
	go func() {
		_, err := rt.StartHistory(id, time.Now().Add(-360*24*time.Hour), false, "")
		answered <- err
	}()
	select {
	case err := <-answered:
		rt.lifecycle.Unlock()
		t.Fatalf("the call answered before the lifecycle mutex was free: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	rt.mux.Lock()
	rt.envs[id].gen = dense
	rt.mux.Unlock()
	rt.lifecycle.Unlock()

	err := <-answered
	rangeError := &HistoryRangeError{}
	if !errors.As(err, &rangeError) {
		t.Fatalf("expected the swapped generation to be refused, got %v", err)
	}
	if engine.callCount() != 0 {
		t.Error("the refused run still started the engine")
	}
	//and the environment was not touched for it
	rt.mux.RLock()
	env := rt.envs[id]
	rt.mux.RUnlock()
	env.mux.Lock()
	underHistory := env.underHistory
	env.mux.Unlock()
	if underHistory {
		t.Error("the refused run still took the environment away from the live simulation")
	}
}

// TestAHistoryRunWaitsForACommandInFlight: a command does not run on the
// environment context, so cancelling the runners leaves it in flight. One that
// has passed the gate but not yet reached the state would otherwise write a
// value of the present into the virtual state, and publish it live.
func TestAHistoryRunWaitsForACommandInFlight(t *testing.T) {
	const id = "env-hist-command-wait"
	//the engine is held for the whole test: it is the handover that clears the
	//flag the assertions below read, so a run that were allowed to finish would
	//make them race it
	engine := newFakeHistoryEngine()
	rt := startRuntimeWithEngine(t, testConfig(time.Hour), newFakeEnvironments(historyTestEnvironment(id)), newFakeStates(), &fakePublisher{}, engine.run)

	rt.mux.RLock()
	env := rt.envs[id]
	rt.mux.RUnlock()

	//the gate as HandleCommand takes it, held over the window between the check
	//and the dispatch that the mutex of the state does not cover
	accepted, reason := env.enterCommand()
	if !accepted {
		t.Fatalf("the environment refused a command before any run: %v", reason)
	}

	answered := make(chan error, 1)
	go func() {
		_, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, "")
		answered <- err
	}()
	select {
	case err := <-answered:
		t.Fatalf("the run started while a command dispatch was in flight: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	env.leaveCommand()
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("unable to start the history run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the run never started after the command finished")
	}

	//and a command arriving now is refused rather than waited for. The engine is
	//still held, so the run cannot have handed the environment back underneath
	//this.
	if accepted, reason = env.enterCommand(); accepted {
		env.leaveCommand()
		t.Error("a command was accepted while the environment stands at a past instant")
	} else if !strings.Contains(reason, "history") {
		t.Errorf("expected the refusal to name the run, got %q", reason)
	}
	close(engine.release)
	waitForHistory(t, rt, id)
}

// TestTheHandoverAlwaysWritesTheStateItHandsOver: the flusher may have written
// the state during the run and cleared the flag with it, and the handover would
// then be a no-op - which leaves the promise "the state is on disk before a live
// tick can move it" resting on a write that happened at some earlier instant.
func TestTheHandoverAlwaysWritesTheStateItHandsOver(t *testing.T) {
	const id = "env-hist-forced-write"
	engine := newFakeHistoryEngine()
	states := newFakeStates()
	//a short flush interval, so the flusher certainly writes while the run is in
	//flight and clears the flag
	rt := startRuntimeWithEngine(t, testConfig(20*time.Millisecond), newFakeEnvironments(historyTestEnvironment(id)), states, &fakePublisher{}, engine.run)

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	<-engine.entered

	rt.mux.RLock()
	env := rt.envs[id]
	rt.mux.RUnlock()
	//wait until the flusher has taken everything there was to write
	if !waitFor(5*time.Second, func() bool {
		env.mux.Lock()
		defer env.mux.Unlock()
		return !env.dirty
	}) {
		t.Fatal("the flusher never caught up with the state of the run")
	}
	before := len(states.savedFor(id))

	close(engine.release)
	waitForHistory(t, rt, id)

	if after := len(states.savedFor(id)); after <= before {
		t.Errorf("the handover wrote nothing: %d saves before, %d after", before, after)
	}
}

// TestARestartedRuntimeTakesRunsAndJobsAgain: the stop flag and the registry
// belong to one incarnation. Carried over, a restarted runtime refuses every run
// and every backfill for good, and answers status calls out of a registry
// describing a runtime that no longer exists. What survives a restart is the
// stored record, which is where the status of the finished run comes from.
func TestARestartedRuntimeTakesRunsAndJobsAgain(t *testing.T) {
	const id = "env-hist-restart"
	engine := newFakeHistoryEngine()
	close(engine.release)
	envs := newFakeEnvironments(testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 3600, hourlyProfile())))
	rt := newRuntime(testConfig(time.Hour), envs, newFakeStates(), nil, newFakeHistoryJobs(), &fakePublisher{})
	rt.historyEngine = engine.run

	first, cancelFirst := context.WithCancel(context.Background())
	if err := rt.Start(first); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	waitForHistory(t, rt, id)
	rt.Stop()
	cancelFirst()

	second, cancelSecond := context.WithCancel(context.Background())
	t.Cleanup(cancelSecond)
	if err := rt.Start(second); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	//the registry of the previous incarnation is gone: what is known about its
	//run is what the store holds, and that run was over before the restart
	rt.historyMux.Lock()
	registered := len(rt.histories)
	rt.historyMux.Unlock()
	if registered != 0 {
		t.Errorf("the new runtime carried %d runs over from the registry of the old one", registered)
	}
	status, err := rt.HistoryStatusOf(id)
	if err != nil {
		t.Errorf("the finished run of the previous incarnation is not known any more: %v", err)
	}
	if status.State != HistoryDone {
		t.Errorf("expected the stored run of the previous incarnation to be done, got %v", status.State)
	}
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Errorf("the restarted runtime refuses a history run: %v", err)
	}
	waitForHistory(t, rt, id)
	//and the same for the neighbouring registry, which carried the same defect
	if _, err := rt.BackfillStatusOf(id); !errors.Is(err, ErrNoBackfill) {
		t.Errorf("the new runtime answered out of the backfill registry of the old one: %v", err)
	}
	if _, err := rt.StartBackfill(id, backfillFrom, backfillTo); err != nil {
		t.Errorf("the restarted runtime refuses a backfill: %v", err)
	}
	waitForBackfill(t, rt, id)
}

// TestACumulativeMeterContinuesFromTheHistoryRunIntoTheLiveSimulation is the
// reason the mode exists: the reconstructed meter and the live one are one ramp,
// where a backfill leaves two with a step between them.
func TestACumulativeMeterContinuesFromTheHistoryRunIntoTheLiveSimulation(t *testing.T) {
	const id = "env-hist-ramp"
	//an hourly rate of 3600 makes one second worth exactly 1
	channel := profileChannel("ch-1", serviceRefOf(id), 1, domain.ProfileSource{Base: 3600, Cumulative: true})
	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(testEnvironment(id, channel)), newFakeStates(), publisher)

	if _, err := rt.StartHistory(id, time.Now().Add(-65*time.Second), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	status := waitForHistory(t, rt, id)
	if status.State != HistoryDone {
		t.Fatalf("expected the run to be done, it is %v (%v)", status.State, status.Error)
	}
	handover := time.Now()

	rt.mux.RLock()
	env := rt.envs[id]
	rt.mux.RUnlock()
	env.mux.Lock()
	reconstructed, _ := asFloat(env.assetStates(testAssetId)["ch-1"])
	env.mux.Unlock()
	if reconstructed < 10 {
		t.Fatalf("the run reconstructed a meter of only %v over ten seconds", reconstructed)
	}

	if !waitFor(5*time.Second, func() bool { return len(liveEventsAfter(publisher, handover)) > 0 }) {
		t.Fatal("the live simulation did not publish after the run")
	}
	first := liveEventsAfter(publisher, handover)[0]
	value, ok := first.value.(float64)
	if !ok {
		t.Fatalf("expected a number, got %T", first.value)
	}
	//one ramp: the live meter carries on from where the run left it, one tick's
	//share higher, rather than starting a second ramp from zero
	if value <= reconstructed || value > reconstructed+3 {
		t.Errorf("the first live reading was %v, expected the meter to carry on from %v", value, reconstructed)
	}
}

// TestTheChangeTriggerDoesNotRepublishAfterAHistoryRun: the comparison base the
// run books is the persisted one, so the live channel compares against what the
// run last published instead of publishing again at once - and the heartbeat owes
// only the rest of the gap.
func TestTheChangeTriggerDoesNotRepublishAfterAHistoryRun(t *testing.T) {
	const id = "env-hist-cov-live"
	channel := profileChannel("ch-1", serviceRefOf(id), 600, flatProfile(230, 0))
	channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 5, EvaluateIntervalSeconds: 1}
	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(testEnvironment(id, channel)), newFakeStates(), publisher)

	if _, err := rt.StartHistory(id, time.Now().Add(-65*time.Second), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	if status := waitForHistory(t, rt, id); status.State != HistoryDone {
		t.Fatalf("expected the run to be done, it is %v (%v)", status.State, status.Error)
	}
	handover := time.Now()

	rt.mux.RLock()
	env := rt.envs[id]
	rt.mux.RUnlock()
	env.mux.Lock()
	booked, known := env.state.LastPublished["ch-1"]
	env.mux.Unlock()
	if !known || booked.Value != 230 {
		t.Fatalf("expected the run's last publish to be booked, got %#v (known %v)", booked, known)
	}

	//three seconds of live evaluations on a value that does not move: nothing may
	//go out, because the base is the one the run left and the heartbeat is ten
	//minutes away
	time.Sleep(3 * time.Second)
	if extra := liveEventsAfter(publisher, handover); len(extra) != 0 {
		t.Errorf("the live channel published %d readings although nothing moved: %v", len(extra), extra)
	}
}

// ---------------------------------------------------------------------------
// the run survives a restart
// ---------------------------------------------------------------------------

// sameStoredInstant compares an instant with one that has been through the
// store: a bson datetime comes back in UTC at millisecond precision, so nothing
// finer than a millisecond survives and Equal is the wrong question.
func sameStoredInstant(a time.Time, b time.Time) bool {
	difference := a.Sub(b)
	if difference < 0 {
		difference = -difference
	}
	return difference < time.Millisecond
}

// historyRunningRecord is a stored run of one environment that is still running.
func historyRunningRecord(id string, def domain.Environment, checkpoint *repo.HistoryCheckpoint) repo.HistoryJobRecord {
	from := time.Now().Add(-2 * time.Hour)
	return repo.HistoryJobRecord{
		EnvironmentId: id,
		State:         repo.HistoryJobRunning,
		From:          from,
		To:            from.Add(time.Hour),
		StartedAt:     from,
		Published:     5,
		Definition:    def,
		Checkpoint:    checkpoint,
	}
}

// TestAStoredHistoryRunIsResumedOnStart is what the checkpoints are for: a
// service that comes back finds the run in the store and continues it from where
// it stood, against the definition it was started with.
func TestAStoredHistoryRunIsResumedOnStart(t *testing.T) {
	const id = "env-hist-resumed-start"
	//two seconds, so the live simulation is visibly running once the run is over
	//while its first tick cannot land in the microseconds before the resume takes
	//the environment away
	def := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 2, flatProfile(230, 0)))
	checkpoint := &repo.HistoryCheckpoint{
		Position:   time.Now().Add(-90 * time.Minute),
		Ticks:      map[string]int64{"ch-1": 1800},
		Channels:   map[string]repo.HistoryChannelMemory{"ch-1": {Published: 5}},
		LastValues: map[string]float64{"ch-1": 815},
		State: repo.RuntimeState{
			Assets: map[string]map[string]interface{}{testAssetId: {"meter": 4711.0}},
		},
	}
	jobs := newFakeHistoryJobs()
	record := historyRunningRecord(id, def, checkpoint)
	if err := jobs.Save(t.Context(), record); err != nil {
		t.Fatal(err)
	}

	engine := newFakeHistoryEngine()
	engine.result = HistoryResult{Published: 9}
	publisher := &fakePublisher{}
	rt := startRuntimeWithJobs(t, testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), jobs, publisher, engine.run)

	select {
	case <-engine.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the stored run was not resumed by the start")
	}

	//the environment belongs to the run before the live simulation can publish a
	//reading of the present into a window the run is still finishing
	rt.mux.RLock()
	env := rt.envs[id]
	rt.mux.RUnlock()
	if env == nil {
		t.Fatal("the environment of the resumed run is not running")
	}
	env.mux.Lock()
	underHistory := env.underHistory
	meter, _ := asFloat(env.assetStates(testAssetId)["meter"])
	cached := env.lastValues["ch-1"]
	env.mux.Unlock()
	if !underHistory {
		t.Error("the resumed run does not own its environment")
	}
	if published := publisher.count(); published != 0 {
		t.Errorf("the live simulation published %d readings before the resumed run took the environment", published)
	}
	if meter != 4711 {
		t.Errorf("the environment holds %v, expected the state of the checkpoint", meter)
	}
	if cached != 815 {
		t.Errorf("the value cache holds %v, expected the one of the checkpoint", cached)
	}

	engine.mux.Lock()
	resumed := engine.resumed
	window := engine.window
	engine.mux.Unlock()
	if resumed == nil || resumed.Ticks["ch-1"] != 1800 {
		t.Errorf("the engine was not handed the stored checkpoint: %#v", resumed)
	}
	if !sameStoredInstant(window[0], record.From) || !sameStoredInstant(window[1], record.To) {
		t.Errorf("the resumed run got the window %v, expected the stored %v to %v", window, record.From, record.To)
	}
	//and what the run had already published is what a status call reports
	status, err := rt.HistoryStatusOf(id)
	if err != nil || status.State != HistoryRunning || status.Published != 5 {
		t.Errorf("expected the resumed run to be reported as running with its counters, got %#v (%v)", status, err)
	}

	handover := time.Now()
	close(engine.release)
	final := waitForHistory(t, rt, id)
	if final.State != HistoryDone {
		t.Errorf("expected the resumed run to finish, it is %v (%v)", final.State, final.Error)
	}
	stored, ok := jobs.recordFor(id)
	if !ok || stored.State != string(HistoryDone) || stored.FinishedAt == nil {
		t.Errorf("the finished run was not stored: %#v", stored)
	}
	if stored.Checkpoint != nil {
		t.Error("a run that is over must not keep a checkpoint to resume from")
	}
	if !waitFor(10*time.Second, func() bool { return len(liveEventsAfter(publisher, handover)) > 0 }) {
		t.Error("the live simulation did not publish after the resumed run")
	}
}

// TestAnEnvironmentWithAStoredRunStartsNoLiveRunner: the resume takes the
// environment over anyway, and a live runner that ran until it did would publish
// readings of the present into the window the run is still simulating - a window
// the run then hands over as the live state. Both the environment start and the
// resume load their series with a budget of minutes, so that is not a window of
// microseconds.
//
// The window under test is the one between the environment being built and the
// resume taking it over, so the store is held in the resume's own path rather
// than in the list of runs it starts from: gating the list would stop the start
// before any environment exists, where there is nothing for a runner to publish.
func TestAnEnvironmentWithAStoredRunStartsNoLiveRunner(t *testing.T) {
	const id = "env-hist-no-live-start"
	//a one second channel, so a runner that had been started would have published
	//several readings inside the window the slow store below keeps open
	def := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 1, flatProfile(230, 0)))
	from := time.Now().Add(-2 * time.Minute)
	clock := &eventClock{}
	jobs := newFakeHistoryJobs()
	jobs.clock = clock
	jobs.resumeGate = make(chan struct{})
	if err := jobs.Save(t.Context(), repo.HistoryJobRecord{
		EnvironmentId: id, State: repo.HistoryJobRunning, From: from, To: from.Add(time.Minute),
		StartedAt: from, Definition: def,
	}); err != nil {
		t.Fatal(err)
	}

	publisher := &fakePublisher{clock: clock}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, jobs, publisher)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	started := make(chan error, 1)
	go func() { started <- rt.Start(ctx) }()

	//the environment is built and published, and the resume is held at the write
	//of its count: from here on a runner of this environment would be ticking
	if !waitFor(10*time.Second, func() bool {
		rt.mux.RLock()
		defer rt.mux.RUnlock()
		return rt.envs[id] != nil
	}) {
		t.Fatal("the start never built the environment of the stored run")
	}
	//longer than the channel's interval: a live runner would have ticked by now
	time.Sleep(1500 * time.Millisecond)
	if live := publisher.liveEvents(); len(live) != 0 {
		t.Errorf("the live simulation published %d readings while a stored run was waiting to be resumed", len(live))
	}
	close(jobs.resumeGate)
	if err := <-started; err != nil {
		t.Fatalf("unable to start the runtime: %v", err)
	}
	t.Cleanup(rt.Stop)

	final := waitForHistory(t, rt, id)
	if final.State != HistoryDone {
		t.Fatalf("expected the resumed run to finish, it is %v (%v)", final.State, final.Error)
	}
	run := publisher.timestampedEvents()
	if len(run) == 0 {
		t.Fatal("the resumed run published nothing, so there is no order to compare against")
	}
	for _, event := range publisher.liveEvents() {
		if event.seq < run[0].seq {
			t.Errorf("a live reading went out before the first reading of the resumed run")
			break
		}
	}
	//and the live simulation runs again once the run is over
	if !waitFor(10*time.Second, func() bool { return len(publisher.liveEvents()) > 0 }) {
		t.Error("the live simulation did not publish after the resumed run")
	}
}

// TestAResumedRunCountsItsResumeBeforeItRuns: the count is the guard against a
// run that takes the service down on every start, so it has to be in the store
// before the run gets going - a run that never returns would otherwise be
// resumed for ever.
func TestAResumedRunCountsItsResumeBeforeItRuns(t *testing.T) {
	const id = "env-hist-resume-counted"
	def := historyTestEnvironment(id)
	jobs := newFakeHistoryJobs()
	record := historyRunningRecord(id, def, nil)
	if err := jobs.Save(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	engine := newFakeHistoryEngine()
	engine.result = HistoryResult{Published: 4}
	rt := startRuntimeWithJobs(t, testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(),
		jobs, &fakePublisher{}, engine.run)

	select {
	case <-engine.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the stored run was not resumed by the start")
	}
	stored, ok := jobs.recordFor(id)
	if !ok {
		t.Fatal("the record is gone")
	}
	if stored.Resumes != 1 {
		t.Errorf("the resume was counted as %d, expected 1", stored.Resumes)
	}
	//and nothing else about the record changed: the count goes in with the whole
	//record, definition and window included
	if stored.State != repo.HistoryJobRunning || stored.Definition.Id != id ||
		!sameStoredInstant(stored.From, record.From) || !sameStoredInstant(stored.To, record.To) {
		t.Errorf("the count rewrote the record: %#v", stored)
	}

	close(engine.release)
	final := waitForHistory(t, rt, id)
	if final.State != HistoryDone {
		t.Fatalf("expected the resumed run to finish, it is %v (%v)", final.State, final.Error)
	}
	//a run that is over carries no count anybody would act on
	if again, _ := jobs.recordFor(id); again.Resumes != 0 {
		t.Errorf("the finished run still carries %d resumes", again.Resumes)
	}
}

// TestARunResumedTooOftenIsClosedAsFailed: a run that takes the service down
// while it is being resumed would come back with every start. After three
// attempts the run is given up on, and its environment starts live as any other.
func TestARunResumedTooOftenIsClosedAsFailed(t *testing.T) {
	const id = "env-hist-resume-exhausted"
	//a one second channel, so the live simulation is visibly running afterwards
	def := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 1, flatProfile(230, 0)))
	jobs := newFakeHistoryJobs()
	record := historyRunningRecord(id, def, &repo.HistoryCheckpoint{
		Position: time.Now().Add(-time.Hour),
		Ticks:    map[string]int64{"ch-1": 5},
		Channels: map[string]repo.HistoryChannelMemory{"ch-1": {}},
	})
	record.Resumes = maxHistoryResumes
	if err := jobs.Save(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	engine := newFakeHistoryEngine()
	close(engine.release)
	publisher := &fakePublisher{}
	rt := startRuntimeWithJobs(t, testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(),
		jobs, publisher, engine.run)

	stored, ok := jobs.recordFor(id)
	if !ok {
		t.Fatal("the record was dropped rather than closed, so nothing says what became of the run")
	}
	if stored.State != string(HistoryFailed) || stored.FinishedAt == nil {
		t.Errorf("expected the run to be closed as failed, got %#v", stored)
	}
	if !strings.Contains(stored.Error, "three times") {
		t.Errorf("expected the reason to say what happened, got %q", stored.Error)
	}
	if stored.Checkpoint != nil {
		t.Error("a run nobody is going to continue keeps no checkpoint")
	}
	if engine.callCount() != 0 {
		t.Error("the run was resumed although it had been resumed three times already")
	}

	rt.mux.RLock()
	env := rt.envs[id]
	rt.mux.RUnlock()
	if env == nil {
		t.Fatal("the environment of the closed run is not running")
	}
	env.mux.Lock()
	underHistory := env.underHistory
	env.mux.Unlock()
	if underHistory {
		t.Error("the environment is owned by a run that nobody started")
	}
	if !waitFor(10*time.Second, func() bool { return len(publisher.liveEvents()) > 0 }) {
		t.Error("the environment of the closed run does not tick")
	}
}

// TestTheTerminalRecordOfAHistoryRunIsRetried: that write is what keeps the next
// start from resuming a run that is in fact over, so a store that is briefly
// unreachable must not be enough to lose it.
func TestTheTerminalRecordOfAHistoryRunIsRetried(t *testing.T) {
	const id = "env-hist-terminal-retry"
	jobs := newFakeHistoryJobs()
	attempts := 0
	attemptMux := sync.Mutex{}
	jobs.saveErrIf = func(record repo.HistoryJobRecord, n int) error {
		if record.State == repo.HistoryJobRunning {
			//the write at the start of the run
			return nil
		}
		attemptMux.Lock()
		attempts++
		failing := attempts <= 2
		attemptMux.Unlock()
		if failing {
			return errors.New("the store is unreachable")
		}
		return nil
	}
	engine := newFakeHistoryEngine()
	engine.result = HistoryResult{Published: 3}
	close(engine.release)
	rt := startRuntimeWithJobs(t, testConfig(time.Hour), newFakeEnvironments(historyTestEnvironment(id)),
		newFakeStates(), jobs, &fakePublisher{}, engine.run)

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	status := waitForHistory(t, rt, id)
	if status.State != HistoryDone {
		t.Fatalf("expected the run to be done, it is %v (%v)", status.State, status.Error)
	}
	stored, ok := jobs.recordFor(id)
	if !ok || stored.State != string(HistoryDone) || stored.FinishedAt == nil {
		t.Errorf("the outcome was not stored although the third attempt was taken: %#v", stored)
	}
	attemptMux.Lock()
	taken := attempts
	attemptMux.Unlock()
	if taken != 3 {
		t.Errorf("the terminal write was attempted %d times, expected three", taken)
	}
	//every attempt is bounded on its own, and well below the budget of an
	//ordinary store call: the whole retry runs with the lifecycle mutex held, so
	//nothing else of the service moves while a store that hangs is waited for
	budgets := jobs.saveBudgetsOfState(id, HistoryDone)
	if len(budgets) != 3 {
		t.Fatalf("expected three bounded attempts, got the budgets %v", budgets)
	}
	whole := time.Duration(0)
	for _, budget := range budgets {
		if budget <= 0 || budget > historyTerminalSaveTimeout {
			t.Errorf("an attempt of the terminal write was given %v, expected at most %v", budget, historyTerminalSaveTimeout)
		}
		whole += budget
	}
	for _, wait := range historyTerminalSaveWaits {
		whole += wait
	}
	if whole > storeTimeout {
		t.Errorf("the terminal write can hold the lifecycle mutex for %v, more than one store call takes", whole)
	}
}

// TestTheRunIsStoredBeforeTheLiveRunnersPublish: the handover reads the
// definition, loads the series and starts the runners, which can take minutes. A
// record still saying running while the live simulation publishes again would be
// resumed by the next start as a run that is over.
func TestTheRunIsStoredBeforeTheLiveRunnersPublish(t *testing.T) {
	const id = "env-hist-store-before-live"
	clock := &eventClock{}
	jobs := newFakeHistoryJobs()
	jobs.clock = clock
	publisher := &fakePublisher{clock: clock}
	def := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 1, flatProfile(230, 0)))
	engine := newFakeHistoryEngine()
	engine.result = HistoryResult{Published: 2}
	close(engine.release)
	rt := startRuntimeWithJobs(t, testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(),
		jobs, publisher, engine.run)

	//at once, so the live runners of the start have not ticked yet: every live
	//reading of this test belongs to the simulation the handover starts
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	//from here on a write takes longer than the interval of the channel above, so
	//a handover that went first would have let a live reading out before the
	//terminal write landed
	jobs.mux.Lock()
	jobs.saveDelay = 1500 * time.Millisecond
	jobs.mux.Unlock()
	status := waitForHistory(t, rt, id)
	if status.State != HistoryDone {
		t.Fatalf("expected the run to be done, it is %v (%v)", status.State, status.Error)
	}
	terminal := jobs.saveSeqOfState(id, HistoryDone)
	if terminal == 0 {
		t.Fatal("the finished run was never stored")
	}
	if !waitFor(15*time.Second, func() bool { return len(publisher.liveEvents()) > 0 }) {
		t.Fatal("the live simulation did not publish after the run")
	}
	live := publisher.liveEvents()
	if live[0].seq < terminal {
		t.Errorf("a live reading (%d) went out before the run was stored as done (%d)", live[0].seq, terminal)
	}
}

// TestAnAbortIsStoredBeforeTheRunEnds: the terminal write may fail, and a record
// left running would be resumed by the next start - publishing the rest of a
// window somebody stopped. The abort is therefore stored where it is decided.
func TestAnAbortIsStoredBeforeTheRunEnds(t *testing.T) {
	const id = "env-hist-abort-first"
	jobs := newFakeHistoryJobs()
	writes := 0
	writeMux := sync.Mutex{}
	jobs.saveErrIf = func(record repo.HistoryJobRecord, n int) error {
		if record.State == repo.HistoryJobRunning {
			return nil
		}
		writeMux.Lock()
		writes++
		first := writes == 1
		writeMux.Unlock()
		if first {
			//the abort's own write; every terminal write after it fails
			return nil
		}
		return errors.New("the store is unreachable")
	}
	engine := newFakeHistoryEngine()
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(historyTestEnvironment(id)),
		newFakeStates(), nil, jobs, &fakePublisher{})
	rt.historyEngine = engine.run
	first, cancelFirst := context.WithCancel(context.Background())
	t.Cleanup(cancelFirst)
	if err := rt.Start(first); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	<-engine.entered
	if _, err := rt.CancelHistory(id); err != nil {
		t.Fatalf("unable to abort the run: %v", err)
	}

	//stored before the run has even ended
	stored, ok := jobs.recordFor(id)
	if !ok || stored.State != string(HistoryCancelled) || stored.FinishedAt == nil {
		t.Fatalf("the abort was not stored where it was decided: %#v", stored)
	}
	if stored.Checkpoint != nil {
		t.Error("an aborted run keeps no checkpoint: nothing is going to continue it")
	}

	close(engine.release)
	status := waitForHistory(t, rt, id)
	if status.State != HistoryCancelled {
		t.Errorf("expected the run to be cancelled, it is %v", status.State)
	}
	if again, _ := jobs.recordFor(id); again.State != string(HistoryCancelled) {
		t.Errorf("the record of the aborted run is %v although every terminal write failed", again.State)
	}
	rt.Stop()

	//and the next start leaves it alone
	resumed := newFakeHistoryEngine()
	close(resumed.release)
	rt.historyEngine = resumed.run
	second, cancelSecond := context.WithCancel(context.Background())
	t.Cleanup(cancelSecond)
	if err := rt.Start(second); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)
	if resumed.callCount() != 0 {
		t.Error("the aborted run was resumed by the next start")
	}
}

// TestAnEnvironmentIsGivenBackWhenItsRunCannotBeResumed: the start builds the
// environment of a stored run with no runners at all, because the resume is
// about to take it over. A resume that then does not happen would leave it
// standing still until the next edit.
func TestAnEnvironmentIsGivenBackWhenItsRunCannotBeResumed(t *testing.T) {
	const id = "env-hist-resume-refused"
	//a one second channel, so a simulation that runs is visible
	def := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 1, flatProfile(230, 0)))
	publisher := &fakePublisher{}
	rt := startRuntimeWithJobs(t, testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(),
		newFakeHistoryJobs(), publisher, nil)

	//exactly what the start of a resumed run leaves behind: an environment that
	//is owned by a run and has no runners
	rt.lifecycle.Lock()
	rt.stopRunners(id)
	rt.mux.RLock()
	env := rt.envs[id]
	rt.mux.RUnlock()
	if env == nil {
		rt.lifecycle.Unlock()
		t.Fatal("the environment is not running")
	}
	env.markUnderHistory()
	rt.giveUpOnResuming(env)
	rt.lifecycle.Unlock()

	env.mux.Lock()
	underHistory := env.underHistory
	env.mux.Unlock()
	if underHistory {
		t.Error("the environment is still owned by a run nobody resumed")
	}
	before := len(publisher.liveEvents())
	if !waitFor(10*time.Second, func() bool { return len(publisher.liveEvents()) > before }) {
		t.Error("the environment does not tick after the resume was given up on")
	}
	if err := rt.SetState(id, repo.StateChange{Assets: map[string]map[string]interface{}{testAssetId: {"x": 1.0}}}); err != nil {
		t.Errorf("the environment still refuses a state change: %v", err)
	}
}

// TestAResumeThatIsRefusedSpendsNoneOfTheThreeAttempts: the count is what closes
// a run that takes the service down while it is being picked up, so it may only
// be spent by a resume that really got going. A resume the registry refuses
// hands the environment to the live simulation instead, and counting that would
// give up on a window after three shutdowns that never ran it.
func TestAResumeThatIsRefusedSpendsNoneOfTheThreeAttempts(t *testing.T) {
	const id = "env-hist-resume-refused-count"
	//a one second channel, so the live simulation is visible afterwards
	def := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 1, flatProfile(230, 0)))
	jobs := newFakeHistoryJobs()
	publisher := &fakePublisher{}
	rt := startRuntimeWithJobs(t, testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(),
		jobs, publisher, nil)

	//stored after the start, so this record is resumed by the call below rather
	//than by the start itself
	record := historyRunningRecord(id, def, nil)
	record.Resumes = 1
	if err := jobs.Save(t.Context(), record); err != nil {
		t.Fatal(err)
	}

	//the environment as a start leaves it for a run it is going to resume, and a
	//registry that takes no further run: the race a shutdown causes
	rt.lifecycle.Lock()
	rt.stopRunners(id)
	rt.mux.RLock()
	env := rt.envs[id]
	rt.mux.RUnlock()
	if env == nil {
		rt.lifecycle.Unlock()
		t.Fatal("the environment is not running")
	}
	env.markUnderHistory()
	rt.historyMux.Lock()
	rt.historiesStopped = true
	rt.historyMux.Unlock()
	rt.resumeHistory(t.Context(), record)
	rt.historyMux.Lock()
	rt.historiesStopped = false
	rt.historyMux.Unlock()
	rt.lifecycle.Unlock()

	stored, ok := jobs.recordFor(id)
	if !ok {
		t.Fatal("the record is gone")
	}
	if stored.Resumes != 1 {
		t.Errorf("the refused resume spent one of the three attempts, the count stands at %d", stored.Resumes)
	}
	if stored.State != repo.HistoryJobRunning {
		t.Errorf("the refused resume closed the run: %#v", stored)
	}
	env.mux.Lock()
	underHistory := env.underHistory
	env.mux.Unlock()
	if underHistory {
		t.Error("the environment is still owned by a run nobody resumed")
	}
	before := len(publisher.liveEvents())
	if !waitFor(10*time.Second, func() bool { return len(publisher.liveEvents()) > before }) {
		t.Error("the environment does not tick after the resume was refused")
	}
}

// TestAShutdownRacingADeletionDeletesTheRecord: a stopped runtime skips the
// handover, which is where the deletion used to be noticed - so the record of a
// deleted environment, definition and all, would be written back and picked up
// by the next start.
func TestAShutdownRacingADeletionDeletesTheRecord(t *testing.T) {
	const id = "env-hist-delete-race"
	jobs := newFakeHistoryJobs()
	engine := newFakeHistoryEngine()
	envs := newFakeEnvironments(historyTestEnvironment(id))
	rt := startRuntimeWithJobs(t, testConfig(time.Hour), envs, newFakeStates(), jobs, &fakePublisher{}, engine.run)

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	<-engine.entered
	if err := envs.Delete(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	//the deletion and the shutdown cannot be ordered from outside, so the state a
	//Stop that has already returned leaves behind is taken here: the run is ended
	//by the deletion path and the runtime is no longer running when the handover
	//gets the lifecycle mutex
	rt.lifecycle.Lock()
	rt.cancelHistory(id)
	rt.running = false
	rt.lifecycle.Unlock()

	if !waitFor(15*time.Second, func() bool {
		_, known := jobs.recordFor(id)
		return !known
	}) {
		stored, _ := jobs.recordFor(id)
		t.Errorf("the record of the deleted environment is still in the store: %#v", stored.State)
	}
	if written := jobs.savesOf(id); len(written) != 1 {
		t.Errorf("expected the write at the start and nothing after it, got %d", len(written))
	}
	//given back, so the cleanup can stop this runtime the way every other test does
	rt.lifecycle.Lock()
	rt.running = true
	rt.lifecycle.Unlock()
}

// TestAStoredHistoryRunOfAMissingEnvironmentIsClosed: the record would otherwise
// be picked up by every start from now on, against a definition of an
// environment this service does not run.
func TestAStoredHistoryRunOfAMissingEnvironmentIsClosed(t *testing.T) {
	const gone = "env-hist-gone"
	const alive = "env-hist-still-here"
	jobs := newFakeHistoryJobs()
	if err := jobs.Save(t.Context(), historyRunningRecord(gone, historyTestEnvironment(gone), nil)); err != nil {
		t.Fatal(err)
	}
	engine := newFakeHistoryEngine()
	close(engine.release)
	rt := startRuntimeWithJobs(t, testConfig(time.Hour), newFakeEnvironments(historyTestEnvironment(alive)),
		newFakeStates(), jobs, &fakePublisher{}, engine.run)

	stored, ok := jobs.recordFor(gone)
	if !ok {
		t.Fatal("the record was dropped rather than closed, so nothing says what became of the run")
	}
	if stored.State != string(HistoryCancelled) || stored.FinishedAt == nil {
		t.Errorf("expected the run to be closed as cancelled, got %#v", stored)
	}
	if !strings.Contains(stored.Error, "no longer exists") {
		t.Errorf("expected the reason to name the missing environment, got %q", stored.Error)
	}
	if engine.callCount() != 0 {
		t.Error("a run was started for an environment that is not here")
	}
	status, err := rt.HistoryStatusOf(gone)
	if err != nil || status.State != HistoryCancelled {
		t.Errorf("expected the closed run to be readable, got %#v (%v)", status, err)
	}
}

// TestAShutdownSuspendsAHistoryRunAndTheNextStartResumesIt is the whole point of
// the checkpoints: a rollout in the middle of a year long run costs the current
// chunk, not the run.
func TestAShutdownSuspendsAHistoryRunAndTheNextStartResumesIt(t *testing.T) {
	const id = "env-hist-suspend"
	def := historyTestEnvironment(id)
	jobs := newFakeHistoryJobs()
	position := time.Now().Add(-30 * time.Minute)
	engine := newFakeHistoryEngine()
	engine.boundary = &repo.HistoryJobProgress{
		Position:  position,
		Published: 7,
		Checkpoint: repo.HistoryCheckpoint{
			Position: position,
			Ticks:    map[string]int64{"ch-1": 3},
			Channels: map[string]repo.HistoryChannelMemory{"ch-1": {Published: 7}},
			State: repo.RuntimeState{
				Assets: map[string]map[string]interface{}{testAssetId: {"meter": 99.0}},
			},
		},
	}

	first, cancelFirst := context.WithCancel(context.Background())
	t.Cleanup(cancelFirst)
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, jobs, &fakePublisher{})
	rt.historyEngine = engine.run
	if err := rt.Start(first); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	<-engine.entered
	rt.Stop()
	//the handover needs the lifecycle mutex Stop holds, so it finishes after it -
	//and it leaves the status running, exactly as the store has it
	waitForHistoryHandover(t, rt, id)
	if status, err := rt.HistoryStatusOf(id); err != nil || status.State != HistoryRunning {
		t.Errorf("expected the suspended run to stay running, got %#v (%v)", status, err)
	}

	stored, ok := jobs.recordFor(id)
	if !ok {
		t.Fatal("the suspended run is not in the store")
	}
	if stored.State != repo.HistoryJobRunning {
		t.Errorf("a shutdown suspends the run rather than ending it, but it is stored as %v", stored.State)
	}
	if stored.FinishedAt != nil {
		t.Error("a suspended run must not be stored as finished")
	}
	if stored.Checkpoint == nil || stored.Checkpoint.Ticks["ch-1"] != 3 {
		t.Fatalf("the checkpoint of the last boundary is what a resume continues from, got %#v", stored.Checkpoint)
	}

	//and the next start picks it up and finishes it
	resumed := newFakeHistoryEngine()
	resumed.result = HistoryResult{Published: 21}
	close(resumed.release)
	rt.historyEngine = resumed.run
	second, cancelSecond := context.WithCancel(context.Background())
	t.Cleanup(cancelSecond)
	if err := rt.Start(second); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	final := waitForHistory(t, rt, id)
	if final.State != HistoryDone || final.Published != 21 {
		t.Errorf("expected the resumed run to finish, got %#v", final)
	}
	resumed.mux.Lock()
	continued := resumed.resumed
	resumed.mux.Unlock()
	if continued == nil || continued.Ticks["ch-1"] != 3 {
		t.Errorf("the second start did not continue from the stored checkpoint: %#v", continued)
	}
	if again, _ := jobs.recordFor(id); again.State != string(HistoryDone) || again.Checkpoint != nil {
		t.Errorf("the finished run was not stored: %#v", again)
	}
}

// TestAnAbortedHistoryRunIsStoredAsCancelled: an abort is the caller's decision
// and has to survive the restart it might be followed by - a record left running
// would be resumed and would publish the rest of a window somebody stopped.
func TestAnAbortedHistoryRunIsStoredAsCancelled(t *testing.T) {
	const id = "env-hist-abort-stored"
	jobs := newFakeHistoryJobs()
	position := time.Now().Add(-30 * time.Minute)
	engine := newFakeHistoryEngine()
	engine.boundary = &repo.HistoryJobProgress{
		Position:   position,
		Checkpoint: repo.HistoryCheckpoint{Position: position, Ticks: map[string]int64{"ch-1": 3}},
	}
	rt := startRuntimeWithJobs(t, testConfig(time.Hour), newFakeEnvironments(historyTestEnvironment(id)),
		newFakeStates(), jobs, &fakePublisher{}, engine.run)

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	<-engine.entered
	if _, err := rt.CancelHistory(id); err != nil {
		t.Fatalf("unable to abort the run: %v", err)
	}
	close(engine.release)
	status := waitForHistory(t, rt, id)
	if status.State != HistoryCancelled {
		t.Fatalf("expected the run to be cancelled, it is %v", status.State)
	}

	stored, ok := jobs.recordFor(id)
	if !ok {
		t.Fatal("the aborted run is not in the store")
	}
	if stored.State != string(HistoryCancelled) || stored.FinishedAt == nil {
		t.Errorf("expected the abort to be stored as a finished run, got %#v", stored)
	}
	if stored.Checkpoint != nil {
		t.Error("an aborted run keeps no checkpoint: nothing is going to continue it")
	}
}

// TestAbortingAFinishedRunLeavesItsOutcomeAlone: the registry keeps a run after
// it ended, so an abort arriving then still finds it. Storing cancelled over it
// would rewrite the outcome of a run that really finished - and answer the
// caller with the state the run had before it was written.
func TestAbortingAFinishedRunLeavesItsOutcomeAlone(t *testing.T) {
	const id = "env-hist-abort-after-done"
	jobs := newFakeHistoryJobs()
	engine := newFakeHistoryEngine()
	engine.result = HistoryResult{Published: 7}
	close(engine.release)
	rt := startRuntimeWithJobs(t, testConfig(time.Hour), newFakeEnvironments(historyTestEnvironment(id)),
		newFakeStates(), jobs, &fakePublisher{}, engine.run)

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	final := waitForHistory(t, rt, id)
	if final.State != HistoryDone {
		t.Fatalf("expected the run to finish, it is %v (%v)", final.State, final.Error)
	}
	writes := len(jobs.savesOf(id))

	status, err := rt.CancelHistory(id)
	if err != nil {
		t.Fatalf("the abort of a finished run is not an error, got %v", err)
	}
	if status.State != HistoryDone || status.Published != 7 {
		t.Errorf("expected the outcome of the finished run, got %#v", status)
	}
	if status.FinishedAt == nil || final.FinishedAt == nil || !status.FinishedAt.Equal(*final.FinishedAt) {
		t.Errorf("the abort moved the instant the run ended at, got %v against %v", status.FinishedAt, final.FinishedAt)
	}
	if stored, ok := jobs.recordFor(id); !ok || stored.State != string(HistoryDone) || stored.Published != 7 {
		t.Errorf("the stored outcome was rewritten by the abort: %#v", stored)
	}
	if again := len(jobs.savesOf(id)); again != writes {
		t.Errorf("the abort of a finished run wrote %d further records", again-writes)
	}
	if status, err = rt.HistoryStatusOf(id); err != nil || status.State != HistoryDone {
		t.Errorf("expected the run to stay done, got %#v (%v)", status, err)
	}
}

// TestAResumeThatReachesABoundaryClearsTheResumeCount: the guard exists for a
// run that takes the service down while it is being picked up, and a run that
// reached a chunk boundary has got past exactly that. Counting its earlier
// resumes for ever would close a long run that is making progress.
func TestAResumeThatReachesABoundaryClearsTheResumeCount(t *testing.T) {
	const id = "env-hist-resume-boundary-clears"
	def := historyTestEnvironment(id)
	position := time.Now().Add(-30 * time.Minute)
	jobs := newFakeHistoryJobs()
	record := historyRunningRecord(id, def, nil)
	record.Resumes = maxHistoryResumes - 1
	if err := jobs.Save(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	engine := newFakeHistoryEngine()
	engine.boundary = &repo.HistoryJobProgress{
		Position:   position,
		Checkpoint: repo.HistoryCheckpoint{Position: position, Ticks: map[string]int64{"ch-1": 3}},
		AtBoundary: true,
	}
	rt := startRuntimeWithJobs(t, testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(),
		jobs, &fakePublisher{}, engine.run)

	select {
	case <-engine.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the stored run was not resumed by the start")
	}
	//the resume itself is counted, and the boundary the run then reaches clears
	//it: the guard bounds the resumes that reached none
	counted := false
	for _, written := range jobs.savesOf(id) {
		if written.Resumes == maxHistoryResumes {
			counted = true
		}
	}
	if !counted {
		t.Errorf("the resume was not counted before the run got going: %#v", jobs.savesOf(id))
	}
	if !waitFor(10*time.Second, func() bool {
		stored, ok := jobs.recordFor(id)
		return ok && stored.Resumes == 0
	}) {
		stored, _ := jobs.recordFor(id)
		t.Errorf("the boundary left the resume count at %d", stored.Resumes)
	}
	if stored, _ := jobs.recordFor(id); stored.State != repo.HistoryJobRunning || stored.Checkpoint == nil {
		t.Errorf("the boundary write touched more than the count: %#v", stored)
	}

	close(engine.release)
	if final := waitForHistory(t, rt, id); final.State != HistoryDone {
		t.Errorf("expected the resumed run to finish, it is %v (%v)", final.State, final.Error)
	}
}

// TestTheStatusOfAHistoryRunSurvivesARestart: the registry belongs to one
// incarnation, so what a caller reads afterwards comes out of the record - the
// per channel breakdown included, which is the other direction of the same
// mapping.
func TestTheStatusOfAHistoryRunSurvivesARestart(t *testing.T) {
	const id = "env-hist-status-restart"
	jobs := newFakeHistoryJobs()
	engine := newFakeHistoryEngine()
	close(engine.release)
	engine.result = HistoryResult{
		Published: 12,
		Failed:    1,
		LastError: "the platform refused this reading",
		Channels: []HistoryChannelStatus{{
			ChannelId: "ch-1", AssetId: testAssetId, Name: "ch-1",
			Publishable: false, Reason: "no time path", Published: 12, Silent: 3, Failed: 1,
			LastError: "the platform refused this reading",
		}},
	}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(historyTestEnvironment(id)), newFakeStates(), nil, jobs, &fakePublisher{})
	rt.historyEngine = engine.run

	first, cancelFirst := context.WithCancel(context.Background())
	t.Cleanup(cancelFirst)
	if err := rt.Start(first); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	before := waitForHistory(t, rt, id)
	rt.Stop()

	second, cancelSecond := context.WithCancel(context.Background())
	t.Cleanup(cancelSecond)
	if err := rt.Start(second); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	after, err := rt.HistoryStatusOf(id)
	if err != nil {
		t.Fatalf("the run of the previous incarnation is not known any more: %v", err)
	}
	if after.State != before.State || after.Published != before.Published || after.Failed != before.Failed {
		t.Errorf("the stored status is %#v, the run reported %#v", after, before)
	}
	if after.LastError != before.LastError || after.Error != before.Error {
		t.Errorf("the stored messages are %q / %q, expected %q / %q", after.LastError, after.Error, before.LastError, before.Error)
	}
	if !sameStoredInstant(after.From, before.From) || !sameStoredInstant(after.To, before.To) || after.FinishedAt == nil {
		t.Errorf("the stored window is %v to %v, expected %v to %v", after.From, after.To, before.From, before.To)
	}
	if len(after.Channels) != 1 || !reflect.DeepEqual(after.Channels[0], before.Channels[0]) {
		t.Errorf("the per channel breakdown did not survive: %#v against %#v", after.Channels, before.Channels)
	}
	//and an abort of a run that is over reports it rather than inventing one
	cancelled, err := rt.CancelHistory(id)
	if err != nil || cancelled.State != HistoryDone {
		t.Errorf("expected the stored outcome, got %#v (%v)", cancelled, err)
	}
}

// TestRemovingAnEnvironmentDeletesItsStoredHistoryRun: the run of an environment
// that is gone must not be left in the store, or a start would find a record
// whose definition describes nothing.
func TestRemovingAnEnvironmentDeletesItsStoredHistoryRun(t *testing.T) {
	const id = "env-hist-remove-record"
	jobs := newFakeHistoryJobs()
	engine := newFakeHistoryEngine()
	envs := newFakeEnvironments(historyTestEnvironment(id))
	rt := startRuntimeWithJobs(t, testConfig(time.Hour), envs, newFakeStates(), jobs, &fakePublisher{}, engine.run)

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	<-engine.entered
	if _, ok := jobs.recordFor(id); !ok {
		t.Fatal("the run was not stored when it started")
	}
	if err := envs.Delete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	rt.Remove(id)
	close(engine.release)
	waitForHistory(t, rt, id)

	if !waitFor(10*time.Second, func() bool {
		_, ok := jobs.recordFor(id)
		return !ok
	}) {
		t.Error("the record of the deleted environment is still in the store")
	}
}

// TestAHistoryRunThatCannotBeStoredIsRefused: a run nothing remembers cannot be
// resumed, and at the moment of the write nothing has been stopped for it - so
// refusing costs a caller one error and leaves the simulation as it was.
func TestAHistoryRunThatCannotBeStoredIsRefused(t *testing.T) {
	const id = "env-hist-store-fails"
	jobs := newFakeHistoryJobs()
	jobs.saveErr = errors.New("the store is unreachable")
	engine := newFakeHistoryEngine()
	close(engine.release)
	publisher := &fakePublisher{}
	//a one second channel, so the live simulation is visibly still running
	def := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 1, flatProfile(230, 0)))
	rt := startRuntimeWithJobs(t, testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), jobs, publisher, engine.run)

	_, err := rt.StartHistory(id, time.Now().Add(-time.Hour), false, "")
	if !errors.Is(err, jobs.saveErr) {
		t.Fatalf("expected the store's error, got %v", err)
	}
	if engine.callCount() != 0 {
		t.Error("the refused run still started the engine")
	}
	if _, err = rt.HistoryStatusOf(id); !errors.Is(err, ErrNoHistory) {
		t.Errorf("the refused run was left in the registry: %v", err)
	}

	rt.mux.RLock()
	env := rt.envs[id]
	rt.mux.RUnlock()
	env.mux.Lock()
	underHistory := env.underHistory
	env.mux.Unlock()
	if underHistory {
		t.Error("the refused run still took the environment away from the live simulation")
	}
	before := publisher.count()
	if !waitFor(5*time.Second, func() bool { return publisher.count() > before }) {
		t.Error("the live simulation stopped over a run that was refused")
	}
	//and a run is accepted again once the store is back
	jobs.mux.Lock()
	jobs.saveErr = nil
	jobs.mux.Unlock()
	if _, err = rt.StartHistory(id, time.Now().Add(-time.Hour), false, ""); err != nil {
		t.Errorf("expected the run to be accepted once the store answers, got %v", err)
	}
	waitForHistory(t, rt, id)
}

// TestAStoredHistoryRunIsClosedByAnAbortEvenWithoutARun: after a Start that
// resumed every running record this cannot be one that is still going, so there
// is nothing to stop - but the document has to be finished, or a GET would keep
// reporting a run nothing is working on.
func TestAStoredHistoryRunIsClosedByAnAbortEvenWithoutARun(t *testing.T) {
	const id = "env-hist-orphan"
	jobs := newFakeHistoryJobs()
	if err := jobs.Save(t.Context(), historyRunningRecord(id, historyTestEnvironment(id), nil)); err != nil {
		t.Fatal(err)
	}
	//deliberately not started: the registry is empty and the store is all there is
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), nil, jobs, &fakePublisher{})

	status, err := rt.CancelHistory(id)
	if err != nil {
		t.Fatalf("unable to close the stored run: %v", err)
	}
	if status.State != HistoryCancelled || status.FinishedAt == nil {
		t.Errorf("expected the abort to report a cancelled run, got %#v", status)
	}
	stored, ok := jobs.recordFor(id)
	if !ok || stored.State != string(HistoryCancelled) {
		t.Errorf("the abort was not stored: %#v", stored)
	}
	if stored.Definition.Id != id {
		t.Errorf("the definition of the run was dropped by the abort: %#v", stored.Definition)
	}

	//and a store that cannot be read is not "there was no run": the api answers
	//500 for it rather than 404
	jobs.mux.Lock()
	jobs.loadErr = errors.New("the store is unreachable")
	jobs.mux.Unlock()
	if _, err = rt.HistoryStatusOf(id); err == nil || errors.Is(err, ErrNoHistory) {
		t.Errorf("expected the store's error to be handed on, got %v", err)
	}
	if _, err = rt.CancelHistory(id); err == nil || errors.Is(err, ErrNoHistory) {
		t.Errorf("expected the store's error to be handed on, got %v", err)
	}
}

// TestAStoredHistoryRunIsKeptWhenItsEnvironmentOnlyFailedToStart: an environment
// whose runtime state could not be read is not started and comes back on the
// next start. Its run must survive that - closing it would throw away a window
// that is still resumable, over a database that was briefly unavailable.
func TestAStoredHistoryRunIsKeptWhenItsEnvironmentOnlyFailedToStart(t *testing.T) {
	const id = "env-hist-unstarted"
	def := historyTestEnvironment(id)
	jobs := newFakeHistoryJobs()
	if err := jobs.Save(t.Context(), historyRunningRecord(id, def, nil)); err != nil {
		t.Fatal(err)
	}
	states := newFakeStates()
	states.loadErr = errors.New("the state is unreadable")
	engine := newFakeHistoryEngine()
	close(engine.release)
	rt := startRuntimeWithJobs(t, testConfig(time.Hour), newFakeEnvironments(def), states, jobs, &fakePublisher{}, engine.run)

	if gen := genOf(rt, id); gen != nil {
		t.Fatal("the environment was started although its state could not be read, so this proves nothing")
	}
	stored, ok := jobs.recordFor(id)
	if !ok {
		t.Fatal("the record is gone")
	}
	if stored.State != repo.HistoryJobRunning || stored.FinishedAt != nil {
		t.Errorf("the run of an environment that only failed to start was closed: %#v", stored)
	}
	if engine.callCount() != 0 {
		t.Error("a run was started for an environment that is not running")
	}
}
