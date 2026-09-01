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
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// historyFrom is a window that has definitely passed, so the engine tests never
// race the wall clock.
var historyFrom = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

// historyFixture builds the engine's preconditions by hand: a runtime that was
// never started, an environment that has been reset and seeded exactly as
// StartHistory does, and no live runners to collide with.
func historyFixture(t *testing.T, def domain.Environment, series map[string][]dataset.Point, publisher *fakePublisher) (*Runtime, *environment, *generation) {
	t.Helper()
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, publisher)
	gen := newGeneration(def, series)
	env := &environment{id: def.Id, gen: gen, state: repo.RuntimeState{EnvironmentId: def.Id}}
	env.resetForHistory()
	//seeded with the window start, exactly as StartHistory does
	env.seed(gen, historyFrom)
	return rt, env, gen
}

// runEngine runs exactly the window given and fails the test if the engine
// itself broke. runEngineChasing is the shape a live run has, where the end was
// the present when it was asked for.
func runEngine(t *testing.T, rt *Runtime, env *environment, gen *generation, from time.Time, to time.Time) HistoryResult {
	t.Helper()
	return runEngineWith(t, rt, env, gen, from, to, keepTheWindow)
}

func runEngineChasing(t *testing.T, rt *Runtime, env *environment, gen *generation, from time.Time, to time.Time) HistoryResult {
	t.Helper()
	return runEngineWith(t, rt, env, gen, from, to, chaseTheClock)
}

func runEngineWith(t *testing.T, rt *Runtime, env *environment, gen *generation, from time.Time, to time.Time, chase bool) HistoryResult {
	t.Helper()
	result, err := rt.runHistory(t.Context(), env, gen, from, to, chase, nil)
	if err != nil {
		t.Fatalf("the history run failed: %v", err)
	}
	return result
}

// historyValues returns the values one service received, in publish order.
func historyValues(publisher *fakePublisher, serviceRef string) []float64 {
	result := []float64{}
	for _, event := range publisher.backfilled(serviceRef) {
		number, ok := event.value.(float64)
		if !ok {
			continue
		}
		result = append(result, number)
	}
	return result
}

func resultFor(t *testing.T, result HistoryResult, channelId string) HistoryChannelStatus {
	t.Helper()
	for _, channel := range result.Channels {
		if channel.ChannelId == channelId {
			return channel
		}
	}
	t.Fatalf("the run reported nothing about %v: %#v", channelId, result.Channels)
	return HistoryChannelStatus{}
}

// TestAHistoryRunIsReproducibleFromTheSeedAndTheWindow is what makes a run
// usable at all: the same document and window produce the same series and the
// same end state, so a simulation can be repeated after the fact.
func TestAHistoryRunIsReproducibleFromTheSeedAndTheWindow(t *testing.T) {
	const id = "env-hist-det"
	def := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 3600, hourlyProfile()))
	def.Seed = 4711

	run := func(seed int64) ([]float64, repo.RuntimeState) {
		document := def
		document.Seed = seed
		publisher := &fakePublisher{}
		rt, env, gen := historyFixture(t, document, nil, publisher)
		runEngine(t, rt, env, gen, historyFrom, historyFrom.Add(24*time.Hour))
		env.mux.Lock()
		defer env.mux.Unlock()
		return historyValues(publisher, serviceRefOf(id)), env.snapshot()
	}

	first, firstState := run(4711)
	second, secondState := run(4711)
	if len(first) != 25 {
		t.Fatalf("expected 25 hourly readings over a day, got %d", len(first))
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two runs published %v and %v", first, second)
	}
	if !reflect.DeepEqual(firstState, secondState) {
		t.Fatalf("two runs ended in %#v and %#v", firstState, secondState)
	}

	//and a different seed has to differ, or the comparison above holds for any
	//two runs
	third, _ := run(4712)
	if reflect.DeepEqual(first, third) {
		t.Error("two seeds produced the same series, so the seed is not reaching the profile")
	}
}

// TestAHistoryRunCoversEveryStepOfEveryGridFromTheStart pins the arithmetic of
// the heap: one due event at the window start, one per whole step after it, none
// past the end.
func TestAHistoryRunCoversEveryStepOfEveryGridFromTheStart(t *testing.T) {
	const id = "env-hist-grid"
	publisher := &fakePublisher{}
	//deliberately not a whole number of intervals: 25h30m on an hourly channel
	to := historyFrom.Add(25*time.Hour + 30*time.Minute)
	rt, env, gen := historyFixture(t, testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 3600, hourlyProfile())), nil, publisher)

	result := runEngine(t, rt, env, gen, historyFrom, to)

	events := publisher.backfilled(serviceRefOf(id))
	if len(events) != 26 {
		t.Fatalf("expected 26 readings over 25h30m on an hourly channel, got %d", len(events))
	}
	for i, event := range events {
		want := historyFrom.Add(time.Duration(i) * time.Hour)
		if !event.at.Equal(want) {
			t.Fatalf("reading %d was stamped %v, expected %v", i, event.at, want)
		}
	}
	if result.Published != 26 {
		t.Errorf("the result counted %d published steps, %d readings went out", result.Published, len(events))
	}
	if !result.Position.Equal(historyFrom.Add(25 * time.Hour)) {
		t.Errorf("the run ended at %v, expected the last due instant", result.Position)
	}
}

// TestAHistoryRunNeverOvershootsThePresent: the virtual clock is clamped once at
// the start. Beyond now, scheduleAt clamps and a replay goes silent, so a run
// that overshot would produce a stretch of made-up readings and then stop.
func TestAHistoryRunNeverOvershootsThePresent(t *testing.T) {
	const id = "env-hist-clamp"
	publisher := &fakePublisher{}
	rt, env, gen := historyFixture(t, testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 1, flatProfile(230, 0))), nil, publisher)

	from := time.Now().Add(-5 * time.Second)
	//an hour into the future: without the clamp this would be 3605 readings
	runEngine(t, rt, env, gen, from, time.Now().Add(time.Hour))

	events := publisher.backfilled(serviceRefOf(id))
	if len(events) == 0 {
		t.Fatal("the run published nothing at all")
	}
	if len(events) > 20 {
		t.Fatalf("expected a handful of readings up to now, got %d - the run overshot the present", len(events))
	}
	tolerance := time.Now().Add(time.Second)
	for _, event := range events {
		if event.at.After(tolerance) {
			t.Errorf("a reading was stamped %v, which lies in the future", event.at)
		}
	}
}

// TestAHistoryRunStopsAtTheEndEvenWithAFractionalStart: a window ends at the
// present, so a step that lands after it lands in the future. The whole seconds
// of the start and the end differ by a whole number of steps here while the
// instants do not, which is the case a grid built on unix seconds alone would
// stamp one reading past the end.
func TestAHistoryRunStopsAtTheEndEvenWithAFractionalStart(t *testing.T) {
	const id = "env-hist-fraction"
	publisher := &fakePublisher{}
	rt, env, gen := historyFixture(t, testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 1, flatProfile(230, 0))), nil, publisher)

	from := historyFrom.Add(500 * time.Millisecond)
	to := historyFrom.Add(10 * time.Second)
	runEngine(t, rt, env, gen, from, to)

	events := publisher.backfilled(serviceRefOf(id))
	if len(events) != 10 {
		t.Fatalf("expected 10 readings from %v to %v on a one second grid, got %d", from, to, len(events))
	}
	for i, event := range events {
		if event.at.After(to) {
			t.Errorf("reading %d was stamped %v, past the end of the window at %v", i, event.at, to)
		}
	}
	if last := events[len(events)-1].at; !last.Equal(from.Add(9 * time.Second)) {
		t.Errorf("the last reading sits at %v, expected the last step inside the window", last)
	}
}

// TestAHistoryRunAnchorsALoopingReplayAtTheWindowStart is the dataset seam. The
// anchor is created at the first virtual tick and persisted, so the live channel
// that takes over afterwards keeps playing at the position the run left it at
// instead of restarting the data at the moment the run ended.
func TestAHistoryRunAnchorsALoopingReplayAtTheWindowStart(t *testing.T) {
	const id = "env-hist-anchor"
	source := replaySource(domain.ResampleHold, domain.AnchorLoop)
	channel := datasetChannel(id, source)
	channel.IntervalSeconds = 300
	publisher := &fakePublisher{}
	rt, env, gen := historyFixture(t, testEnvironment(id, channel),
		map[string][]dataset.Point{"ch-1": replayPoints}, publisher)

	runEngine(t, rt, env, gen, historyFrom, historyFrom.Add(time.Hour))

	env.mux.Lock()
	anchor, known := env.state.Anchors["ch-1"]
	env.mux.Unlock()
	if !known || anchor != historyFrom.Unix() {
		t.Fatalf("expected the replay anchor at the window start %d, got %d (known %v)", historyFrom.Unix(), anchor, known)
	}

	//every reading is what the same anchor produces at that instant, which is
	//what the live channel keeps computing after the run
	events := publisher.backfilled(serviceRefOf(id))
	if len(events) != 13 {
		t.Fatalf("expected 13 readings over an hour on a five minute channel, got %d", len(events))
	}
	for i, event := range events {
		want, playable := replayValue(source, replayPoints, historyFrom.Unix(), event.at, 300)
		if !playable {
			t.Fatalf("reading %d at %v is not playable from the window anchor", i, event.at)
		}
		if event.value != want {
			t.Errorf("reading %d at %v was %v, expected %v from an anchor at the window start", i, event.at, event.value, want)
		}
	}
}

// TestAHistoryRunKeepsACumulativeMeterInTheState: the run carries state, which
// is the whole difference to a backfill. The meter it publishes is the one in
// the asset state, so the live channel picks it up where the run left it.
func TestAHistoryRunKeepsACumulativeMeterInTheState(t *testing.T) {
	const id = "env-hist-meter"
	//an hourly rate of 3600 makes one second worth exactly 1
	channel := profileChannel("ch-1", serviceRefOf(id), 60, domain.ProfileSource{Base: 3600, Cumulative: true})
	publisher := &fakePublisher{}
	rt, env, gen := historyFixture(t, testEnvironment(id, channel), nil, publisher)

	runEngine(t, rt, env, gen, historyFrom, historyFrom.Add(time.Hour))

	values := historyValues(publisher, serviceRefOf(id))
	if len(values) != 61 {
		t.Fatalf("expected 61 readings over an hour on a minute channel, got %d", len(values))
	}
	for i, value := range values {
		want := float64(i+1) * 60
		if math.Abs(value-want) > 1e-6 {
			t.Fatalf("reading %d was %v, expected the meter to stand at %v", i, value, want)
		}
	}

	env.mux.Lock()
	stored, _ := asFloat(env.state.Assets[testAssetId]["ch-1"])
	cached := env.lastValues["ch-1"]
	env.mux.Unlock()
	if math.Abs(stored-3660) > 1e-6 {
		t.Errorf("the meter in the state stands at %v, expected the 3660 the last reading published", stored)
	}
	if math.Abs(cached-3660) > 1e-6 {
		t.Errorf("the value cache holds %v, expected the last published 3660", cached)
	}
}

// TestAHistoryRunDiscardsTheStateItStartedFrom: the mode exists to replace the
// live state, so a meter reading left over from the live simulation must not
// become the start of the reconstructed ramp - it is a value from the future.
func TestAHistoryRunDiscardsTheStateItStartedFrom(t *testing.T) {
	const id = "env-hist-discard"
	channel := profileChannel("ch-1", serviceRefOf(id), 60, domain.ProfileSource{Base: 3600, Cumulative: true})
	def := testEnvironment(id, channel)
	publisher := &fakePublisher{}

	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, publisher)
	gen := newGeneration(def, nil)
	env := &environment{id: id, gen: gen, state: repo.RuntimeState{
		EnvironmentId: id,
		Context:       map[string]interface{}{"outside": 21.0},
		Zones:         map[string]map[string]interface{}{},
		Assets:        map[string]map[string]interface{}{testAssetId: {"ch-1": 500000.0}},
		Anchors:       map[string]int64{"ch-1": 1_700_000_000},
		LastPublished: map[string]repo.PublishedValue{"ch-1": {Value: 500000, AtUnix: 1_700_000_000}},
	}}
	env.lastValues = map[string]float64{"ch-1": 500000}

	env.resetForHistory()
	env.seed(gen, historyFrom)
	runEngine(t, rt, env, gen, historyFrom, historyFrom.Add(10*time.Minute))

	values := historyValues(publisher, serviceRefOf(id))
	if len(values) == 0 {
		t.Fatal("the run published nothing")
	}
	if math.Abs(values[0]-60) > 1e-6 {
		t.Errorf("the first reading was %v, expected the ramp to start from 0 rather than from the live meter", values[0])
	}
	env.mux.Lock()
	defer env.mux.Unlock()
	if _, stale := env.state.Anchors["ch-1"]; stale && env.state.Anchors["ch-1"] == 1_700_000_000 {
		t.Error("the run kept the replay anchor of the state it replaced")
	}
	if _, stale := env.state.LastPublished["ch-1"]; stale {
		t.Error("the run kept the comparison base of the state it replaced")
	}
	if env.state.Context["outside"] != nil {
		t.Errorf("the run kept a context value of the state it replaced: %v", env.state.Context["outside"])
	}
}

// TestAHistoryRunReproducesTheChangeTriggerAgainstThePersistedBase is the change
// of value seam. The comparison base is the persisted one, written by the same
// covGate the live channel uses, so what the run last published is exactly what
// the first live evaluation compares against - see the lifecycle test for the
// other half.
func TestAHistoryRunReproducesTheChangeTriggerAgainstThePersistedBase(t *testing.T) {
	const id = "env-hist-cov"
	channel := profileChannel("ch-1", serviceRefOf(id), 600, flatProfile(230, 0))
	channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 5, EvaluateIntervalSeconds: 60}
	publisher := &fakePublisher{}
	rt, env, gen := historyFixture(t, testEnvironment(id, channel), nil, publisher)

	result := runEngine(t, rt, env, gen, historyFrom, historyFrom.Add(time.Hour))

	//one at the start, then one per ten minute heartbeat up to the end
	events := publisher.backfilled(serviceRefOf(id))
	if len(events) != 7 {
		t.Fatalf("expected 7 heartbeats over an hour of a constant value, got %d", len(events))
	}
	for i, event := range events {
		want := historyFrom.Add(time.Duration(i) * 600 * time.Second)
		if !event.at.Equal(want) {
			t.Fatalf("reading %d was stamped %v, expected %v", i, event.at, want)
		}
		if event.value != 230.0 {
			t.Fatalf("reading %d was %v, expected the unchanged 230", i, event.value)
		}
	}

	env.mux.Lock()
	booked, known := env.state.LastPublished["ch-1"]
	env.mux.Unlock()
	last := historyFrom.Add(3600 * time.Second)
	if !known || booked.Value != 230 || booked.AtUnix != last.Unix() {
		t.Errorf("expected the last virtual publish booked as 230 at %d, got %#v", last.Unix(), booked)
	}

	//every step of the evaluation grid is accounted for, and a suppressed one is
	//silent rather than missing
	channelResult := resultFor(t, result, "ch-1")
	steps := backfillTicks(60, historyFrom, historyFrom.Add(time.Hour))
	if channelResult.Published+channelResult.Silent+channelResult.Failed != steps {
		t.Errorf("%d published, %d silent, %d failed do not add up to the %d steps of the grid",
			channelResult.Published, channelResult.Silent, channelResult.Failed, steps)
	}
	if channelResult.Silent != 54 {
		t.Errorf("expected the 54 suppressed evaluations to be counted as silent, got %d", channelResult.Silent)
	}
}

// TestARefusedReadingIsCountedAndNamedWithoutShiftingTheGrid: a run that mostly
// worked still has to say what went wrong, and the heartbeat gap restarts on the
// attempt rather than on the success - otherwise one refusal would make every
// following instant overdue and move the whole remaining grid.
func TestARefusedReadingIsCountedAndNamedWithoutShiftingTheGrid(t *testing.T) {
	const id = "env-hist-refused"
	channel := profileChannel("ch-1", serviceRefOf(id), 600, flatProfile(230, 0))
	channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 5, EvaluateIntervalSeconds: 60}
	refused := historyFrom.Add(600 * time.Second)
	publisher := &fakePublisher{failAt: func(at time.Time) error {
		if at.Equal(refused) {
			return errors.New("the platform refused this reading")
		}
		return nil
	}}
	rt, env, gen := historyFixture(t, testEnvironment(id, channel), nil, publisher)

	result := runEngine(t, rt, env, gen, historyFrom, historyFrom.Add(time.Hour))

	wanted := []time.Duration{0, 1200 * time.Second, 1800 * time.Second,
		2400 * time.Second, 3000 * time.Second, 3600 * time.Second}
	events := publisher.backfilled(serviceRefOf(id))
	if len(events) != len(wanted) {
		t.Fatalf("expected %d readings on the unshifted grid, got %d", len(wanted), len(events))
	}
	for i, offset := range wanted {
		if !events[i].at.Equal(historyFrom.Add(offset)) {
			t.Errorf("reading %d was stamped %v, expected %v", i, events[i].at, historyFrom.Add(offset))
		}
	}
	channelResult := resultFor(t, result, "ch-1")
	if channelResult.Failed != 1 || result.Failed != 1 {
		t.Errorf("expected exactly the one refused step to be counted, got %d (%d overall)", channelResult.Failed, result.Failed)
	}
	if !strings.Contains(channelResult.LastError, "refused") || !strings.Contains(result.LastError, "refused") {
		t.Errorf("expected the refusal to be reported, got %q / %q", channelResult.LastError, result.LastError)
	}
}

// TestAChannelThatCannotPublishStillComputes is the point of running the whole
// environment rather than the channels a backfill can serve: a channel whose
// service takes no timestamp evolves its state and feeds every formula and
// aggregate above it, and only its readings are dropped.
func TestAChannelThatCannotPublishStillComputes(t *testing.T) {
	const id = "env-hist-no-time-path"
	channel := profileChannel("ch-1", serviceRefOf(id), 60, domain.ProfileSource{Base: 3600, Cumulative: true})
	publisher := &fakePublisher{shapeErr: map[string]error{serviceRefOf(id): devices.ErrNoTimePath}}
	rt, env, gen := historyFixture(t, testEnvironment(id, channel), nil, publisher)

	result := runEngine(t, rt, env, gen, historyFrom, historyFrom.Add(10*time.Minute))

	if publisher.count() != 0 {
		t.Errorf("a channel whose service takes no timestamp published %d readings", publisher.count())
	}
	channelResult := resultFor(t, result, "ch-1")
	if channelResult.Publishable {
		t.Error("the channel was treated as publishable")
	}
	if !strings.Contains(channelResult.Reason, devices.ErrNoTimePath.Error()) {
		t.Errorf("expected the reason to name the missing time path, got %q", channelResult.Reason)
	}
	if channelResult.Silent != 11 || channelResult.Published != 0 {
		t.Errorf("expected all 11 steps to be silent, got %#v", channelResult)
	}

	env.mux.Lock()
	defer env.mux.Unlock()
	stored, _ := asFloat(env.state.Assets[testAssetId]["ch-1"])
	if math.Abs(stored-660) > 1e-6 {
		t.Errorf("the meter stands at %v, expected the run to have advanced it to 660 anyway", stored)
	}
}

// TestAChannelThatCannotPublishBooksNoComparisonBase is the other half of it. A
// channel publishing on change must leave last_published empty when nothing went
// out, or the first value the live channel computes would be compared against a
// reading nobody ever received and could stay unpublished for a whole heartbeat.
func TestAChannelThatCannotPublishBooksNoComparisonBase(t *testing.T) {
	const id = "env-hist-no-time-path-cov"
	channel := profileChannel("ch-1", serviceRefOf(id), 600, flatProfile(230, 0))
	channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 5, EvaluateIntervalSeconds: 60}
	publisher := &fakePublisher{shapeErr: map[string]error{serviceRefOf(id): devices.ErrNoTimePath}}
	rt, env, gen := historyFixture(t, testEnvironment(id, channel), nil, publisher)

	runEngine(t, rt, env, gen, historyFrom, historyFrom.Add(time.Hour))

	env.mux.Lock()
	defer env.mux.Unlock()
	if booked, known := env.state.LastPublished["ch-1"]; known {
		t.Errorf("nothing was published, but %#v was booked as the comparison base", booked)
	}
}

// TestASplitChannelRunsItsSourceBeforeItPublishes: both fall on the same instant
// whenever the publish interval is a multiple of the source interval, and a
// publish that went first would send the value of the previous instant.
func TestASplitChannelRunsItsSourceBeforeItPublishes(t *testing.T) {
	const id = "env-hist-split"
	channel := scriptChannel("ch-1", domain.Sensor, 2, serviceRefOf(id),
		"var n = moses.asset.state.get('n') + 1; moses.asset.state.set('n', n); moses.channel.send(n);")
	channel.Source.IntervalSeconds = 1
	publisher := &fakePublisher{}
	rt, env, gen := historyFixture(t, testEnvironment(id, channel), nil, publisher)

	runEngine(t, rt, env, gen, historyFrom, historyFrom.Add(10*time.Second))

	//the source runs at every second and the publish at every other one, so the
	//publish carries the odd counts of its own instant: 1, 3, 5, 7, 9, 11
	values := historyValues(publisher, serviceRefOf(id))
	want := []float64{1, 3, 5, 7, 9, 11}
	if !reflect.DeepEqual(values, want) {
		t.Errorf("expected %v from a source that runs before the publish of the same instant, got %v", want, values)
	}
}

// TestAContextSourceMovesBeforeTheChannelsThatReadIt: a gate that opens at one
// instant has to be open for the channel due at the same instant, or every
// declared programme of a site would start one evaluation late.
func TestAContextSourceMovesBeforeTheChannelsThatReadIt(t *testing.T) {
	const id = "env-hist-context-first"
	//the context replays 0 for thirty seconds and 1 for the next thirty
	points := []dataset.Point{{Unix: 0, Value: 0}, {Unix: 30, Value: 1}, {Unix: 60, Value: 0}}
	schedule := domain.ScheduleSource{
		StateKey: "programm",
		Gate:     &domain.ScheduleGate{ContextKey: "shift", Threshold: 0.5},
		States:   []domain.ScheduleState{{Name: "run", DurationSeconds: 3600, Value: 42}},
	}
	def := testEnvironment(id, scheduleChannel("ch-1", serviceRefOf(id), 1, schedule))
	source := replaySource(domain.ResampleHold, domain.AnchorLoop)
	def.ContextSources = map[string]domain.Source{
		"shift": {Kind: domain.SourceDataset, IntervalSeconds: 1, Dataset: &source},
	}
	publisher := &fakePublisher{}
	rt, env, gen := historyFixture(t, def,
		map[string][]dataset.Point{contextSeriesId("shift"): points}, publisher)

	runEngine(t, rt, env, gen, historyFrom, historyFrom.Add(40*time.Second))

	byInstant := map[int64]float64{}
	for _, event := range publisher.backfilled(serviceRefOf(id)) {
		byInstant[event.at.Unix()] = event.value.(float64)
	}
	rise := historyFrom.Add(30 * time.Second).Unix()
	if value, known := byInstant[rise-1]; !known || value != 0 {
		t.Errorf("expected the machine to stand still one second before the gate opens, got %v (known %v)", value, known)
	}
	if value, known := byInstant[rise]; !known || value != 42 {
		t.Errorf("expected the machine to run at the instant the gate opens, got %v (known %v) - the context source moved after the channel", value, known)
	}

	//the pass is anchored at the rising edge of the virtual clock, not at the
	//moment the run happened to execute
	env.mux.Lock()
	run := env.state.ScheduleRuns["ch-1"]
	env.mux.Unlock()
	if run.PassUnix != rise {
		t.Errorf("the pass is anchored at %d, expected the virtual rising edge %d", run.PassUnix, rise)
	}
}

// TestAScheduleRunsOnTheVirtualClock: a schedule is the one source a backfill
// refuses outright, because it stands where its anchor puts it. The run creates
// that anchor at the virtual start and walks the programme from there.
func TestAScheduleRunsOnTheVirtualClock(t *testing.T) {
	const id = "env-hist-schedule"
	schedule := shortSchedule(
		domain.ScheduleState{Name: "load", DurationSeconds: 10, Value: 100},
		domain.ScheduleState{Name: "idle", DurationSeconds: 10, Value: 10},
	)
	publisher := &fakePublisher{}
	rt, env, gen := historyFixture(t, testEnvironment(id, scheduleChannel("ch-1", serviceRefOf(id), 1, schedule)), nil, publisher)

	runEngine(t, rt, env, gen, historyFrom, historyFrom.Add(40*time.Second))

	values := historyValues(publisher, serviceRefOf(id))
	if len(values) != 41 {
		t.Fatalf("expected 41 readings over 40 seconds on a one second channel, got %d", len(values))
	}
	for i, value := range values {
		want := 100.0
		if (i/10)%2 == 1 {
			want = 10.0
		}
		if value != want {
			t.Fatalf("reading %d (second %d) was %v, expected %v from a programme anchored at the window start", i, i, value, want)
		}
	}

	env.mux.Lock()
	run := env.state.ScheduleRuns["ch-1"]
	env.mux.Unlock()
	//rolled forward by the whole cycles it consumed, which keeps the position
	//and every draw where they were
	if run.StartUnix != historyFrom.Add(40*time.Second).Unix() || run.CycleOffset != 2 {
		t.Errorf("expected the anchor rolled forward to the running cycle, got %#v", run)
	}
}

// TestAnAggregateOfAHistoryRunSumsTheVirtualChildren: an aggregate reads what its
// inputs last produced, so it only adds up inside the run if the children run on
// the same virtual clock and write the same value cache.
func TestAnAggregateOfAHistoryRunSumsTheVirtualChildren(t *testing.T) {
	const id = "env-hist-aggregate"
	//deliberately written with the total ABOVE its sub-meters, which is the
	//natural way to draw a meter tree and the order that used to make the
	//aggregate sum the previous instant: a derived channel runs after every
	//producing one of the same instant, whatever the document order is
	def := treeEnvironment(id,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", serviceOf(id, "total"), 60, energyCharacteristic)}},
		treeAsset{id: "a-sub-1", submeteredBy: "a-total", channels: []domain.Channel{
			cumulativeChannel("ch-sub-1", serviceOf(id, "sub-1"), 60, energyCharacteristic, 3600)}},
		treeAsset{id: "a-sub-2", submeteredBy: "a-total", channels: []domain.Channel{
			cumulativeChannel("ch-sub-2", serviceOf(id, "sub-2"), 60, energyCharacteristic, 7200)}},
	)
	publisher := &fakePublisher{}
	rt, env, gen := historyFixture(t, def, nil, publisher)

	runEngine(t, rt, env, gen, historyFrom, historyFrom.Add(10*time.Minute))

	totals := historyValues(publisher, serviceOf(id, "total"))
	if len(totals) != 11 {
		t.Fatalf("expected 11 totals over ten minutes on a minute channel, got %d", len(totals))
	}
	//the sum is of the meters of its own instant: 60 and 120 per minute
	for i, value := range totals {
		want := float64(i+1) * 180
		if math.Abs(value-want) > 1e-6 {
			t.Fatalf("total %d was %v, expected %v - the aggregate summed a different instant", i, value, want)
		}
	}
}

// TestAFormulaOfAHistoryRunReadsTheChannelsOfItsOwnInstant is the same rule for
// the other derived source: a formula over channel refs reads the value cache,
// which the producing channels of that instant have to have written first.
func TestAFormulaOfAHistoryRunReadsTheChannelsOfItsOwnInstant(t *testing.T) {
	const id = "env-hist-formula-order"
	//the formula is written first in the document, the channel it reads second
	formula := domain.Channel{
		Id: "ch-doubled", Name: "doubled", Direction: domain.Sensor, ExternalRef: serviceRefOf(id) + "-doubled",
		IntervalSeconds: 60,
		Source: domain.Source{Kind: domain.SourceFormula, Formula: &domain.FormulaSource{
			Expression: "2 * a", Inputs: map[string]string{"a": "channel.ch-meter"}}},
	}
	meter := profileChannel("ch-meter", serviceRefOf(id)+"-meter", 60, domain.ProfileSource{Base: 3600, Cumulative: true})
	publisher := &fakePublisher{}
	rt, env, gen := historyFixture(t, testEnvironment(id, formula, meter), nil, publisher)

	runEngine(t, rt, env, gen, historyFrom, historyFrom.Add(5*time.Minute))

	doubled := historyValues(publisher, serviceRefOf(id)+"-doubled")
	if len(doubled) != 6 {
		t.Fatalf("expected 6 readings over five minutes on a minute channel, got %d", len(doubled))
	}
	for i, value := range doubled {
		want := float64(i+1) * 120
		if math.Abs(value-want) > 1e-6 {
			t.Fatalf("reading %d was %v, expected %v - the formula read the previous instant", i, value, want)
		}
	}
}

// cumulativeChannel is a meter that counts at a fixed hourly rate, so a sum over
// two of them is an exact number.
func cumulativeChannel(id string, ref string, interval int64, characteristic string, base float64) domain.Channel {
	channel := profileChannel(id, ref, interval, domain.ProfileSource{Base: base, Cumulative: true})
	channel.CharacteristicId = characteristic
	return channel
}

// TestAHistoryRunStopsWhenItsContextIsCancelled: the run is long, so an abort has
// to take effect at the next due event rather than at the end of the window.
func TestAHistoryRunStopsWhenItsContextIsCancelled(t *testing.T) {
	const id = "env-hist-cancel"
	publisher := &fakePublisher{}
	rt, env, gen := historyFixture(t, testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 1, flatProfile(230, 0))), nil, publisher)

	ctx, cancel := context.WithCancel(t.Context())
	published := 0
	publisher.failAt = func(at time.Time) error {
		published++
		if published == 5 {
			cancel()
		}
		return nil
	}
	result, err := rt.runHistory(ctx, env, gen, historyFrom, historyFrom.Add(time.Hour), keepTheWindow, nil)
	cancel()
	//the engine names the abort itself rather than leaving it to be read off the
	//context afterwards, where a run that had just finished would look cancelled
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the run to report the cancellation, got %v", err)
	}
	if result.Published >= 3601 {
		t.Errorf("the cancelled run still published %d of 3601 readings", result.Published)
	}
	if result.Published == 0 {
		t.Error("the run published nothing at all before it was cancelled")
	}
}

// TestAHistoryRunChasesTheTimeItSpentSimulating: the end of the window is the
// present at the moment of the request, and a run of any length has moved on
// from it by the time it drains. Handing the environment over across that hole
// would put back exactly the step the mode exists to avoid, so the run keeps
// going until it has caught up.
func TestAHistoryRunChasesTheTimeItSpentSimulating(t *testing.T) {
	const id = "env-hist-catchup"
	//a publisher that takes its time, so the run measurably lags the clock
	publisher := &fakePublisher{failAt: func(at time.Time) error {
		time.Sleep(30 * time.Millisecond)
		return nil
	}}
	rt, env, gen := historyFixture(t, testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 1, flatProfile(230, 0))), nil, publisher)

	//61 steps at 30 ms is about 1.8 s of simulation for a window that ends now
	from := time.Now().Add(-60 * time.Second)
	requested := time.Now()
	result := runEngineChasing(t, rt, env, gen, from, requested)

	events := publisher.backfilled(serviceRefOf(id))
	if len(events) < 62 {
		t.Fatalf("expected the run to publish past the %d steps of the requested window, got %d", 61, len(events))
	}
	last := events[len(events)-1].at
	if !last.After(requested.Add(500 * time.Millisecond)) {
		t.Errorf("the last reading sits at %v, which is not past the requested end %v - the run left a hole", last, requested)
	}
	if !result.End.After(requested) {
		t.Errorf("the run reports its end as %v, expected it to have moved past the requested %v", result.End, requested)
	}
	//and it stops rather than following the clock forever
	if last.After(time.Now()) {
		t.Errorf("the last reading at %v lies in the future", last)
	}
}

// TestTheChaseStopsUnlessEachRoundHalvesTheGap: the steps a chase adds are not
// covered by the volume check, so what bounds them is that the rounds are a
// halving sequence and sum to about twice the first one. A round that only
// shaves a little off would multiply the work the check allowed by the number of
// rounds.
func TestTheChaseStopsUnlessEachRoundHalvesTheGap(t *testing.T) {
	huge := time.Duration(math.MaxInt64)
	for name, testCase := range map[string]struct {
		gap     time.Duration
		lastGap time.Duration
		want    bool
	}{
		"the first round, whatever the gap":  {time.Hour, huge, true},
		"a round that halves the gap":        {30 * time.Minute, time.Hour, true},
		"a round that does better than half": {time.Minute, time.Hour, true},
		"a round that only shrinks it":       {59 * time.Minute, time.Hour, false},
		"a round that shrinks it barely":     {time.Hour - time.Nanosecond, time.Hour, false},
		"a gap that grew":                    {2 * time.Hour, time.Hour, false},
		"a gap that is closed enough":        {historyCatchUpSettled, huge, false},
		"a gap just over the settling point": {historyCatchUpSettled + time.Millisecond, huge, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := historyChasesOn(testCase.gap, testCase.lastGap); got != testCase.want {
				t.Errorf("expected %v for a gap of %v after %v, got %v", testCase.want, testCase.gap, testCase.lastGap, got)
			}
		})
	}
}

// TestAHistoryRunOfAWindowThatIsAlreadyOverIsNotExtended: the chase is for a run
// whose end was the present when it was asked for, and the caller says which it
// is. Guessing it from how old the end looks would switch the chase off silently
// for a run whose start took a while.
func TestAHistoryRunOfAWindowThatIsAlreadyOverIsNotExtended(t *testing.T) {
	const id = "env-hist-no-catchup"
	publisher := &fakePublisher{}
	rt, env, gen := historyFixture(t, testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 3600, flatProfile(230, 0))), nil, publisher)

	to := historyFrom.Add(24 * time.Hour)
	result := runEngine(t, rt, env, gen, historyFrom, to)

	if len(publisher.backfilled(serviceRefOf(id))) != 25 {
		t.Fatalf("expected exactly the 25 readings of the window, got %d", len(publisher.backfilled(serviceRefOf(id))))
	}
	if !result.End.Equal(to) {
		t.Errorf("the run ended at %v, expected the requested %v", result.End, to)
	}
}

// TestTheHistoryVolumeCountsEveryGridOfEveryChannel: the run executes every
// channel, publishable or not, and a split channel twice per publish interval.
// Counting only what publishes would wave a run through that then takes hours.
func TestTheHistoryVolumeCountsEveryGridOfEveryChannel(t *testing.T) {
	from := historyFrom
	to := from.Add(time.Hour)

	split := scriptChannel("ch-split", domain.Sensor, 60, "svc-split", "moses.channel.send(1);")
	split.Source.IntervalSeconds = 1
	cov := profileChannel("ch-cov", "svc-cov", 600, flatProfile(230, 0))
	cov.PublishOnChange = &domain.ChangeTrigger{Absolute: 5, EvaluateIntervalSeconds: 60}
	plain := profileChannel("ch-plain", "svc-plain", 3600, flatProfile(230, 0))

	def := testEnvironment("env-hist-volume", split, cov, plain)
	def.ContextSources = map[string]domain.Source{
		"outside": {Kind: domain.SourceProfile, IntervalSeconds: 60, Profile: profilePointer(flatProfile(10, 0))},
	}
	gen := newGeneration(def, nil)

	for name, testCase := range map[string]struct {
		binding int
		want    int64
	}{
		//3601 source steps plus 61 publish steps
		"a split channel is counted on both of its grids": {0, 3662},
		//the heartbeat rides on the evaluation grid
		"a change trigger is counted on its evaluation grid": {1, 61},
		"a plain channel is counted on its publish interval": {2, 2},
	} {
		t.Run(name, func(t *testing.T) {
			if got := historyTicksOf(gen.sensors[testCase.binding], from, to); got != testCase.want {
				t.Errorf("expected %d due events, got %d", testCase.want, got)
			}
		})
	}

	//and the context sources are counted too, or a site whose context ticks every
	//second would slip past the cap
	if err := checkHistoryVolume(gen, from, to); err != nil {
		t.Errorf("expected an hour of this environment to be accepted, got %v", err)
	}
	long := from.Add(-300 * 24 * time.Hour)
	if err := checkHistoryVolume(gen, long, to); err == nil {
		t.Error("expected 300 days of a one second source to be refused")
	} else {
		rangeError := &HistoryRangeError{}
		if !errors.As(err, &rangeError) || !strings.Contains(rangeError.Error(), "simulation steps") {
			t.Errorf("expected a HistoryRangeError naming the step count, got %v", err)
		}
	}
}

func profilePointer(p domain.ProfileSource) *domain.ProfileSource { return &p }

// TestAHistoryWindowThatCannotBeRunIsRefused pins the boundaries. The clock is
// passed in rather than read, so none of these depend on when the test runs.
func TestAHistoryWindowThatCannotBeRunIsRefused(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	for name, testCase := range map[string]struct {
		from     time.Time
		refused  bool
		contains string
	}{
		"a plain past instant":         {now.Add(-7 * day), false, ""},
		"exactly the maximum span":     {now.Add(-MaxBackfillSpan), false, ""},
		"one second over the span":     {now.Add(-MaxBackfillSpan - time.Second), true, "more than the"},
		"exactly now":                  {now, true, "in the past"},
		"in the future":                {now.Add(day), true, "in the past"},
		"before the platform":          {time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), true, "which is not a window"},
		"zero":                         {time.Time{}, true, "required"},
		"one second inside the span":   {now.Add(-MaxBackfillSpan + time.Second), false, ""},
		"exactly the minimum span":     {now.Add(-minHistorySpan), false, ""},
		"one second under the minimum": {now.Add(-minHistorySpan + time.Second), true, "less than the"},
		"a fraction of a second":       {now.Add(-100 * time.Millisecond), true, "less than the"},
	} {
		t.Run(name, func(t *testing.T) {
			from, to, err := validateHistoryWindow(testCase.from, now)
			if !testCase.refused {
				if err != nil {
					t.Fatalf("expected the window to be accepted, got %v", err)
				}
				if !to.Equal(now) {
					t.Errorf("the end has to be the present, got %v", to)
				}
				if !from.Equal(testCase.from) {
					t.Errorf("the start was moved to %v", from)
				}
				return
			}
			rangeError := &HistoryRangeError{}
			if !errors.As(err, &rangeError) {
				t.Fatalf("expected a HistoryRangeError, got %v", err)
			}
			if !strings.Contains(rangeError.Error(), testCase.contains) {
				t.Errorf("expected the reason to mention %q, got %q", testCase.contains, rangeError.Error())
			}
		})
	}
}
