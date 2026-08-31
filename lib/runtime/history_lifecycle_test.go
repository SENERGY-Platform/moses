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
}

func newFakeHistoryEngine() *fakeHistoryEngine {
	return &fakeHistoryEngine{
		entered: make(chan struct{}), left: make(chan struct{}), release: make(chan struct{}),
	}
}

func (this *fakeHistoryEngine) run(ctx context.Context, env *environment, gen *generation, from time.Time, to time.Time, chase bool, progress historyProgress) (HistoryResult, error) {
	this.mux.Lock()
	this.calls++
	this.window = [2]time.Time{from, to}
	this.mux.Unlock()
	this.once.Do(func() { close(this.entered) })
	defer this.leftOnce.Do(func() { close(this.left) })
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
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rt := newRuntime(cfg, envs, states, nil, publisher)
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

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour)); err != nil {
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
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour)); err != nil {
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
			if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour)); err != nil {
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

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour)); err != nil {
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

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour)); err != nil {
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
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(historyTestEnvironment(id)), states, nil, &fakePublisher{})
	rt.historyEngine = engine.run
	if err := rt.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour)); err != nil {
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

	status := waitForHistory(t, rt, id)
	if status.State == HistoryRunning {
		t.Errorf("the run is still running after the runtime stopped: %#v", status)
	}
	//the partial state is not lost: Stop flushes what the engine phase left
	//behind before the handover gets the lifecycle mutex
	if len(states.savedFor(id)) == 0 {
		t.Error("the state the run had reached was never written")
	}
	//and no further run is accepted
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour)); !errors.Is(err, repo.ErrNotRunning) {
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

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	<-engine.entered

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour)); !errors.Is(err, ErrHistoryRunning) {
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
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour)); !errors.Is(err, ErrBackfillRunning) {
		t.Errorf("expected the run to be refused while a backfill is running, got %v", err)
	}
	if engine.callCount() != 0 {
		t.Error("the refused run still started the engine")
	}
	close(publisher.gate)
	waitForBackfill(t, rt, id)

	//and with the job finished the run is allowed
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour)); err != nil {
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
			_, err := rt.StartHistory(id, from)
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

func TestTheHistoryRunOfAnUnknownEnvironmentIsNotRunning(t *testing.T) {
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), &fakePublisher{})
	if _, err := rt.StartHistory("nobody", time.Now().Add(-time.Hour)); !errors.Is(err, repo.ErrNotRunning) {
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

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour)); err != nil {
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

	if _, err := rt.StartHistory(id, time.Now().Add(-2*time.Minute)); err != nil {
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
		_, err := rt.StartHistory(id, time.Now().Add(-360*24*time.Hour))
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
		_, err := rt.StartHistory(id, time.Now().Add(-time.Hour))
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

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour)); err != nil {
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
// describing a runtime that no longer exists.
func TestARestartedRuntimeTakesRunsAndJobsAgain(t *testing.T) {
	const id = "env-hist-restart"
	engine := newFakeHistoryEngine()
	close(engine.release)
	envs := newFakeEnvironments(testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 3600, hourlyProfile())))
	rt := newRuntime(testConfig(time.Hour), envs, newFakeStates(), nil, &fakePublisher{})
	rt.historyEngine = engine.run

	first, cancelFirst := context.WithCancel(context.Background())
	if err := rt.Start(first); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour)); err != nil {
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

	//nothing is known about the run of the previous incarnation
	if _, err := rt.HistoryStatusOf(id); !errors.Is(err, ErrNoHistory) {
		t.Errorf("the new runtime answered out of the registry of the old one: %v", err)
	}
	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour)); err != nil {
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

	if _, err := rt.StartHistory(id, time.Now().Add(-65*time.Second)); err != nil {
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

	if _, err := rt.StartHistory(id, time.Now().Add(-65*time.Second)); err != nil {
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
