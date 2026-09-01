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
	"math"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// monday 2026-08-24 06:00 UTC, the start of a shift
var scheduleT = time.Date(2026, 8, 24, 6, 0, 0, 0, time.UTC)

const scheduleSeed = int64(4711)

// fixedSchedule is a programme whose cycle is exactly 60 seconds, so that every
// boundary in the tests below is a number one can write down.
func fixedSchedule() domain.ScheduleSource {
	return domain.ScheduleSource{
		StateKey: "programm",
		States: []domain.ScheduleState{
			{Name: "idle", DurationSeconds: 10, Value: 400},
			{Name: "setup", DurationSeconds: 20, Value: 2000},
			{Name: "running", DurationSeconds: 30, Value: 9000},
		},
	}
}

// varyingSchedule draws a new duration for every state in every cycle, which is
// the shape the roll-forward has to be invisible in.
func varyingSchedule() domain.ScheduleSource {
	source := fixedSchedule()
	for i := range source.States {
		source.States[i].DurationSpreadPercent = 40
	}
	return source
}

// runAt is a run that has just begun: anchor and pass salt are the same instant,
// which is what the runtime writes at a first start and at every rising edge.
// They part company as soon as the anchor rolls forward.
func runAt(t time.Time) repo.ScheduleRun {
	return repo.ScheduleRun{StartUnix: t.Unix(), PassUnix: t.Unix(), Open: true}
}

// gatedSchedule is the varying programme with a gate on it, which is the shape
// whose draws hang on the pass salt rather than on a constant.
func gatedSchedule() domain.ScheduleSource {
	source := varyingSchedule()
	source.Gate = &domain.ScheduleGate{ContextKey: "shift"}
	return source
}

// ---------------------------------------------------------------------------
// the pure core
// ---------------------------------------------------------------------------

func TestAScheduleWalksItsStatesInTheOrderTheyAreWritten(t *testing.T) {
	source := fixedSchedule()
	run := runAt(scheduleT)
	for _, testCase := range []struct {
		offset int64
		index  int
	}{
		{0, 0}, {9, 0}, {10, 1}, {29, 1}, {30, 2}, {59, 2},
		//and around again
		{60, 0}, {69, 0}, {70, 1},
	} {
		got := scheduleAt(source, scheduleSeed, "ch", run, scheduleT.Unix()+testCase.offset)
		if got.index != testCase.index {
			t.Errorf("%d seconds in: expected state %d (%q), got %d (%q)",
				testCase.offset, testCase.index, source.States[testCase.index].Name,
				got.index, source.States[got.index].Name)
		}
	}
}

func TestAScheduleReportsTheWholeCyclesItWalkedPast(t *testing.T) {
	source := fixedSchedule()
	run := runAt(scheduleT)
	got := scheduleAt(source, scheduleSeed, "ch", run, scheduleT.Unix()+185)
	//three whole cycles of 60 seconds, then 5 seconds into the next one
	if got.consumedCycles != 3 || got.consumedSeconds != 180 {
		t.Errorf("expected 3 cycles worth 180 seconds, got %d worth %d", got.consumedCycles, got.consumedSeconds)
	}
	if got.cycle != 3 || got.index != 0 {
		t.Errorf("expected to stand in state 0 of cycle 3, got state %d of cycle %d", got.index, got.cycle)
	}
	//and the offset the anchor already carries is added rather than replaced
	run.CycleOffset = 100
	if got := scheduleAt(source, scheduleSeed, "ch", run, scheduleT.Unix()+185); got.cycle != 103 {
		t.Errorf("expected cycle 103 with an offset of 100, got %d", got.cycle)
	}
}

// A clock that jumps backwards, or an anchor written by a machine whose clock
// was ahead: the programme has not started yet as far as the anchor knows, and
// anything else would be a position nobody can derive.
func TestAScheduleClampsToItsFirstStateWhenTheClockRanBackwards(t *testing.T) {
	for _, source := range []domain.ScheduleSource{fixedSchedule(), varyingSchedule()} {
		got := scheduleAt(source, scheduleSeed, "ch", runAt(scheduleT), scheduleT.Unix()-3600)
		if got.index != 0 || got.consumedCycles != 0 || got.held {
			t.Errorf("expected the first state and nothing consumed, got %+v", got)
		}
	}
}

func TestARunOnceScheduleHoldsItsLastStateAndConsumesNothing(t *testing.T) {
	for _, source := range []domain.ScheduleSource{fixedSchedule(), varyingSchedule()} {
		source.RunOnce = true
		//well past any drawn cycle: 60 seconds nominal, at most 84 with the spread
		got := scheduleAt(source, scheduleSeed, "ch", runAt(scheduleT), scheduleT.Unix()+5000)
		if got.index != len(source.States)-1 || !got.held {
			t.Errorf("expected the last state to be held, got %+v", got)
		}
		if got.consumedCycles != 0 || got.consumedSeconds != 0 {
			t.Errorf("a single pass has no second cycle to advance into, got %+v", got)
		}
		//and before the end it is an ordinary walk
		if got := scheduleAt(source, scheduleSeed, "ch", runAt(scheduleT), scheduleT.Unix()+5); got.index != 0 || got.held {
			t.Errorf("expected the first state five seconds in, got %+v", got)
		}
	}
}

func TestADrawnDurationIsDeterministicVariesPerCycleAndNeverFallsBelowASecond(t *testing.T) {
	state := domain.ScheduleState{Name: "setup", DurationSeconds: 300, DurationSpreadPercent: 40}
	first := scheduleStateDuration(state, scheduleSeed, "ch|0|1", 0)
	if again := scheduleStateDuration(state, scheduleSeed, "ch|0|1", 0); again != first {
		t.Errorf("the same cycle has to draw the same duration, got %d and %d", first, again)
	}
	if other := scheduleStateDuration(state, scheduleSeed+1, "ch|0|1", 0); other == first {
		t.Error("a different seed has to change the draw")
	}
	varied := false
	for cycle := int64(0); cycle < 100; cycle++ {
		drawn := scheduleStateDuration(state, scheduleSeed, "ch|0|1", cycle)
		if drawn != first {
			varied = true
		}
		if drawn < 180 || drawn > 420 {
			t.Fatalf("40 percent around 300 seconds has to stay in [180,420], got %d in cycle %d", drawn, cycle)
		}
	}
	if !varied {
		t.Error("the duration has to vary from cycle to cycle, otherwise the spread is decoration")
	}

	//the floor: a one second state with almost the whole spread would round to
	//zero for about half of all cycles, and a cycle of length zero is what the
	//walk divides by
	short := domain.ScheduleState{Name: "blip", DurationSeconds: 1, DurationSpreadPercent: 99}
	for cycle := int64(0); cycle < 1000; cycle++ {
		if drawn := scheduleStateDuration(short, scheduleSeed, "ch|0|0", cycle); drawn < 1 {
			t.Fatalf("a drawn duration must never be shorter than a second, got %d in cycle %d", drawn, cycle)
		}
	}
}

// A document that bypassed the api. Validation demands a duration between one
// second and a year and a finite spread, so none of these can be stored - but a
// hand written document must not be able to turn a cycle length negative, which
// is what converting a float that does not fit into an int64 produces and what
// the walk would then divide by.
func TestADrawnDurationStaysAPositiveNumberOfSecondsWhateverTheDocumentSays(t *testing.T) {
	for _, state := range []domain.ScheduleState{
		{Name: "absurd", DurationSeconds: math.MaxInt64},
		{Name: "absurd", DurationSeconds: math.MaxInt64, DurationSpreadPercent: 50},
		{Name: "infinite", DurationSeconds: 100, DurationSpreadPercent: math.Inf(1)},
		{Name: "not a number", DurationSeconds: 100, DurationSpreadPercent: math.NaN()},
		{Name: "negative", DurationSeconds: -5},
		{Name: "zero", DurationSeconds: 0},
	} {
		for _, cycle := range []int64{0, 1, 2, 3} {
			drawn := scheduleStateDuration(state, scheduleSeed, "ch|0|0", cycle)
			if drawn < 1 || drawn > domain.MaxScheduleDurationSeconds {
				t.Errorf("%q drew %d seconds in cycle %d, which is outside [1,%d]",
					state.Name, drawn, cycle, domain.MaxScheduleDurationSeconds)
			}
		}
	}
	//and the walk over such a document still answers rather than dividing by a
	//cycle of no length
	broken := domain.ScheduleSource{StateKey: "programm", States: []domain.ScheduleState{
		{Name: "a", DurationSeconds: -1}, {Name: "b", DurationSeconds: math.MaxInt64, DurationSpreadPercent: 99}}}
	if got := scheduleAt(broken, scheduleSeed, "ch", runAt(scheduleT), scheduleT.Unix()+3600); got.index < 0 || got.index > 1 {
		t.Errorf("expected a state of the programme, got %+v", got)
	}
}

// The finest point of the whole source. A cycling schedule moves its anchor to
// the start of the cycle it is in, so the walk stays short however long the
// environment runs - and that must change neither the state it stands in, nor
// the value it publishes, nor any duration it will draw later. If it did, a
// schedule left running would slowly drift away from the programme it was
// written as, and nothing would look wrong while it happened.
//
// The gated case is here for the same reason and is the sharper one: its draws
// are salted, and a salt read from the anchor instead of from PassUnix would
// redraw every duration of the running pass on every roll.
func TestRollingTheAnchorForwardMovesNeitherThePositionNorTheDraws(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		source domain.ScheduleSource
	}{
		{"free running", varyingSchedule()},
		{"gated", gatedSchedule()},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			source := testCase.source
			fixed := runAt(scheduleT)
			rolled := fixed

			for offset := int64(0); offset < 6*3600; offset += 7 {
				now := scheduleT.Unix() + offset
				want := scheduleAt(source, scheduleSeed, "ch", fixed, now)
				got := scheduleAt(source, scheduleSeed, "ch", rolled, now)

				if got.index != want.index || got.cycle != want.cycle {
					t.Fatalf("%d seconds in: the rolled anchor stands in state %d of cycle %d, the untouched one in state %d of cycle %d",
						offset, got.index, got.cycle, want.index, want.cycle)
				}
				//the whole cycle, not only the state it is in: a draw that moved
				//would change when this state ends even while the index matches
				if a, b := cycleDurations(source, rolled, got.cycle), cycleDurations(source, fixed, want.cycle); !equalDurations(a, b) {
					t.Fatalf("%d seconds in: the durations of cycle %d differ, %v against %v", offset, got.cycle, a, b)
				}
				if scheduleRollsForward(source) && got.consumedCycles > 0 {
					//exactly what executeSchedule persists: the anchor and the
					//cycle counter move, the pass salt does not
					rolled.StartUnix += got.consumedSeconds
					rolled.CycleOffset += got.consumedCycles
				}
			}
			//without this the test would pass on a schedule that never rolls at all
			if rolled.CycleOffset == 0 || rolled.StartUnix == fixed.StartUnix {
				t.Fatalf("the anchor never rolled, so nothing was proven: %+v", rolled)
			}
			if rolled.PassUnix != fixed.PassUnix {
				t.Errorf("the pass salt moved with the anchor: %+v against %+v", rolled, fixed)
			}
		})
	}
}

// cycleDurations is what a cycle of one run is drawn as, through exactly the
// path scheduleAt takes.
func cycleDurations(source domain.ScheduleSource, run repo.ScheduleRun, cycle int64) []int64 {
	durations := make([]int64, len(source.States))
	keys := scheduleDrawKeys("ch", scheduleSalt(source, run), len(source.States))
	scheduleCycle(source, scheduleSeed, keys, cycle, durations)
	return durations
}

func equalDurations(a []int64, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The other half of the salt rule: a gated schedule draws on the instant its
// gate opened, so two shifts do not set up in exactly the same number of
// seconds. That instant is PassUnix and not the anchor, which is what lets the
// anchor roll forward.
func TestAGatedScheduleDrawsItsOwnDurationsPerStart(t *testing.T) {
	source := gatedSchedule()

	monday := runAt(scheduleT)
	tuesday := runAt(scheduleT.Add(24 * time.Hour))
	if equalDurations(cycleDurations(source, monday, 0), cycleDurations(source, tuesday, 0)) {
		t.Error("two openings of the gate have to draw their own durations")
	}
	//the same opening has to reproduce, which is what survives a restart
	if !equalDurations(cycleDurations(source, monday, 0), cycleDurations(source, runAt(scheduleT), 0)) {
		t.Error("the same opening has to draw the same durations")
	}

	//and without a gate the draw does not depend on the anchor at all, which is
	//what makes rolling it forward safe
	free := varyingSchedule()
	if !equalDurations(cycleDurations(free, monday, 0), cycleDurations(free, tuesday, 0)) {
		t.Error("a free running schedule must draw on the cycle number alone")
	}
	//the salt is the pass and not the anchor: a run whose anchor has rolled by
	//two cycles still draws what it drew when the gate opened
	moved := monday
	moved.StartUnix += 120
	moved.CycleOffset += 2
	if !equalDurations(cycleDurations(source, moved, 5), cycleDurations(source, monday, 5)) {
		t.Error("rolling the anchor of a gated run redrew its durations, so the salt is being read from the wrong field")
	}

	if !scheduleRollsForward(source) {
		t.Error("a gated schedule has to roll its anchor forward too, or a gate that stays open walks from the rising edge on every evaluation")
	}
	runOnce := domain.ScheduleSource{RunOnce: true}
	if scheduleRollsForward(runOnce) {
		t.Error("a single pass has no cycle to roll into")
	}
}

// What the roll-forward of a gated run is for. A gate that never closes is not
// exotic - a 24/7 line is a context key at a constant 1 - and without the roll
// the walk from the rising edge grows for as long as the gate stays open, on
// every evaluation, under the environment mutex every other source shares.
func TestAGateThatStaysOpenDoesNotGrowItsWalk(t *testing.T) {
	source := gatedSchedule()
	run := runAt(scheduleT)

	//a month of evaluations an hour apart, on a cycle of about a minute: with
	//the roll-forward each of them walks the last hour, without it the n-th one
	//walks n hours
	const anHour = int64(3600)
	worst := int64(0)
	for hour := int64(1); hour <= 24*30; hour++ {
		position := scheduleAt(source, scheduleSeed, "ch", run, scheduleT.Unix()+hour*anHour)
		if position.consumedCycles > 200 {
			t.Fatalf("evaluation %d walked %d cycles, which is more than the hour since the last one: the anchor is not being rolled forward",
				hour, position.consumedCycles)
		}
		if position.consumedCycles > worst {
			worst = position.consumedCycles
		}
		if scheduleRollsForward(source) && position.consumedCycles > 0 {
			run.StartUnix += position.consumedSeconds
			run.CycleOffset += position.consumedCycles
		}
	}
	if worst == 0 {
		t.Fatal("nothing was walked at all, so the bound above proves nothing")
	}
	if run.PassUnix != scheduleT.Unix() {
		t.Errorf("the pass salt moved during the month: %+v", run)
	}
	//and the anchor really did travel the month, whole cycles at a time
	if run.StartUnix <= scheduleT.Unix() || run.CycleOffset == 0 {
		t.Errorf("the anchor of the gated run never rolled: %+v", run)
	}
}

func TestTheValueSpreadOfAStateStaysInsideItsBand(t *testing.T) {
	state := domain.ScheduleState{Name: "running", Value: 9000, SpreadPercent: 5}
	flat := domain.ScheduleState{Name: "idle", Value: 400}
	for slot := int64(0); slot < 500; slot++ {
		at := scheduleT.Add(time.Duration(slot) * 10 * time.Second)
		got := scheduleValue(state, scheduleSeed, "ch", 10, at)
		if got < 8550 || got > 9450 {
			t.Fatalf("5 percent around 9000 has to stay in [8550,9450], got %v", got)
		}
		if plain := scheduleValue(flat, scheduleSeed, "ch", 10, at); plain != 400 {
			t.Fatalf("a state without spread has to publish its bare value, got %v", plain)
		}
	}
	//stable inside one step, redrawn in the next
	a := scheduleValue(state, scheduleSeed, "ch", 10, scheduleT)
	if b := scheduleValue(state, scheduleSeed, "ch", 10, scheduleT.Add(5*time.Second)); a != b {
		t.Errorf("the value has to be stable inside one evaluation step, got %v and %v", a, b)
	}
	if b := scheduleValue(state, scheduleSeed, "ch", 10, scheduleT.Add(10*time.Second)); a == b {
		t.Error("the next step has to redraw")
	}
}

// A gap no roll-forward ever closed - a clock that jumped by years, or a gate
// somebody left open since. The walk is bounded, and what is left is folded
// into the cycle it stopped at, so the machine keeps running instead of the
// process walking a hundred million cycles under the environment mutex.
func TestAScheduleWithAGapNoAnchorEverClosedKeepsCycling(t *testing.T) {
	source := varyingSchedule()
	got := scheduleAt(source, scheduleSeed, "ch", runAt(scheduleT), scheduleT.Unix()+40*365*24*3600)
	if got.consumedCycles != maxScheduleCycleWalk {
		t.Errorf("expected the walk to stop at its bound, got %d cycles", got.consumedCycles)
	}
	if got.index < 0 || got.index >= len(source.States) {
		t.Errorf("the position has to stay a state of the programme, got %d", got.index)
	}
	//and it is still a pure function: the same gap gives the same answer
	again := scheduleAt(source, scheduleSeed, "ch", runAt(scheduleT), scheduleT.Unix()+40*365*24*3600)
	if again != got {
		t.Errorf("the bounded walk has to stay deterministic, got %+v and %+v", got, again)
	}
}

// ---------------------------------------------------------------------------
// the running channel
// ---------------------------------------------------------------------------

func scheduleChannel(id string, ref string, interval int64, source domain.ScheduleSource) domain.Channel {
	return domain.Channel{
		Id: id, Name: id, Direction: domain.Sensor, ExternalRef: ref, IntervalSeconds: interval,
		Source: domain.Source{Kind: domain.SourceSchedule, Schedule: &source},
	}
}

// shortSchedule is what the runtime tests need: seconds rather than minutes, so
// a test sees several states without waiting for a shift.
func shortSchedule(states ...domain.ScheduleState) domain.ScheduleSource {
	return domain.ScheduleSource{StateKey: "programm", States: states}
}

// runOf reads the persisted anchor out of the running environment. In package,
// under the same mutex the runtime uses: reading it through a flush would make
// every assertion about it a race with the flush interval.
func runOf(t *testing.T, rt *Runtime, envId string, channelId string) (repo.ScheduleRun, bool) {
	t.Helper()
	rt.mux.RLock()
	env := rt.envs[envId]
	rt.mux.RUnlock()
	if env == nil {
		t.Fatalf("the environment %v is not running", envId)
	}
	env.mux.Lock()
	defer env.mux.Unlock()
	run, known := env.state.ScheduleRuns[channelId]
	return run, known
}

// assetStateOf reads one asset state key through the live snapshot, which is
// the same path the state endpoint takes.
func assetStateOf(t *testing.T, rt *Runtime, envId string, key string) interface{} {
	t.Helper()
	snapshot, err := rt.Snapshot(envId)
	if err != nil {
		t.Fatalf("unable to read the state of %v: %v", envId, err)
	}
	return snapshot.State.Assets[testAssetId][key]
}

func TestAScheduleChannelPublishesItsStatesAndNamesThemInTheAssetState(t *testing.T) {
	const envId = "env-schedule"
	source := shortSchedule(
		domain.ScheduleState{Name: "idle", DurationSeconds: 1, Value: 400},
		domain.ScheduleState{Name: "running", DurationSeconds: 2, Value: 9000},
	)
	env := testEnvironment(envId, scheduleChannel("ch-1", serviceRefOf(envId), 1, source))
	publisher := &fakePublisher{}
	runtime := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	seen := map[float64]bool{}
	names := map[string]bool{}
	saw := func() bool {
		for _, value := range publisher.forDevice(deviceRefOf(envId)) {
			number, ok := value.(float64)
			if !ok {
				t.Fatalf("expected a bare number, got %T (%v)", value, value)
			}
			seen[number] = true
		}
		if name, ok := assetStateOf(t, runtime, envId, "programm").(string); ok {
			names[name] = true
		}
		return seen[400] && seen[9000]
	}
	if !waitFor(10*time.Second, saw) {
		t.Fatalf("expected both state values to be published, saw %v", seen)
	}
	for name := range names {
		if name != "idle" && name != "running" {
			t.Errorf("the state key carried %q, which is not a state of the programme", name)
		}
	}
	if len(names) == 0 {
		t.Error("the name of the running state never reached the asset state")
	}
}

// A free running schedule has to keep its walk short: the anchor moves to the
// start of the cycle it is in, and the cycles it skipped keep being counted.
func TestAFreeRunningScheduleRollsItsAnchorForward(t *testing.T) {
	const envId = "env-schedule-roll"
	source := shortSchedule(
		domain.ScheduleState{Name: "a", DurationSeconds: 1, Value: 1},
		domain.ScheduleState{Name: "b", DurationSeconds: 1, Value: 2},
	)
	env := testEnvironment(envId, scheduleChannel("ch-1", serviceRefOf(envId), 1, source))
	publisher := &fakePublisher{}
	runtime := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	first, known := repo.ScheduleRun{}, false
	if !waitFor(4*time.Second, func() bool { first, known = runOf(t, runtime, envId, "ch-1"); return known }) {
		t.Fatal("the schedule never wrote an anchor")
	}
	rolled := func() bool {
		run, _ := runOf(t, runtime, envId, "ch-1")
		return run.CycleOffset > 0
	}
	if !waitFor(10*time.Second, rolled) {
		run, _ := runOf(t, runtime, envId, "ch-1")
		t.Fatalf("the anchor of a free running schedule never rolled forward: %+v", run)
	}
	run, _ := runOf(t, runtime, envId, "ch-1")
	//two states of a second each: the anchor moves by whole cycles only
	if (run.StartUnix-first.StartUnix)%2 != 0 || run.StartUnix <= first.StartUnix {
		t.Errorf("the anchor moved by %d seconds, which is not a whole number of cycles", run.StartUnix-first.StartUnix)
	}
	if run.StartUnix-first.StartUnix != 2*run.CycleOffset {
		t.Errorf("anchor and cycle counter disagree: %+v against %+v", run, first)
	}
	//the pass is written at the start and stands still, here as well as under a
	//gate: nothing reads it while there is no gate, and a gate added to this
	//document later has to find an instant rather than a zero
	if first.PassUnix != first.StartUnix || run.PassUnix != first.PassUnix {
		t.Errorf("the pass of a free running schedule has to be its first anchor and stay there: %+v after %+v", run, first)
	}
}

// A restart continues the programme instead of starting it over. Without the
// persisted anchor a deployment would look like every declared machine of a
// site setting itself up at the same moment.
func TestAScheduleResumesItsProgrammeAfterARestart(t *testing.T) {
	const envId = "env-schedule-restart"
	source := shortSchedule(
		domain.ScheduleState{Name: "charging", DurationSeconds: 1, Value: 10},
		domain.ScheduleState{Name: "charged", DurationSeconds: 3600, Value: 20},
	)
	source.RunOnce = true
	env := testEnvironment(envId, scheduleChannel("ch-1", serviceRefOf(envId), 1, source))
	envs := newFakeEnvironments(env)
	states := newFakeStates()

	first := &fakePublisher{}
	before := startRuntime(t, testConfig(50*time.Millisecond), envs, states, first)
	if !waitFor(8*time.Second, func() bool { return sawValue(first, serviceRefOf(envId), 20.0) }) {
		t.Fatalf("setup: the programme never reached its second state, published %v", valuesOn(first, serviceRefOf(envId)))
	}
	anchor, _ := runOf(t, before, envId, "ch-1")
	before.Stop()

	after := &fakePublisher{}
	resumed := startRuntime(t, testConfig(50*time.Millisecond), envs, states, after)
	if !waitFor(8*time.Second, func() bool { return len(valuesOn(after, serviceRefOf(envId))) >= 2 }) {
		t.Fatal("the schedule did not publish after the restart")
	}
	for _, value := range valuesOn(after, serviceRefOf(envId)) {
		if value == 10.0 {
			t.Fatalf("the programme started over after the restart: %v", valuesOn(after, serviceRefOf(envId)))
		}
	}
	if resumedRun, _ := runOf(t, resumed, envId, "ch-1"); resumedRun.StartUnix != anchor.StartUnix {
		t.Errorf("the anchor was not resumed: %+v after %+v", resumedRun, anchor)
	}
}

// gatedEnvironment is the demonstrator's shape: a shift calendar in the
// context, a machine that only runs while it is open.
func gatedEnvironment(envId string, source domain.ScheduleSource) domain.Environment {
	source.Gate = &domain.ScheduleGate{ContextKey: "shift"}
	env := testEnvironment(envId, scheduleChannel("ch-1", serviceRefOf(envId), 1, source))
	env.Context = map[string]interface{}{"shift": float64(0)}
	return env
}

func setShift(t *testing.T, runtime *Runtime, envId string, value float64) {
	t.Helper()
	if err := runtime.SetState(envId, repo.StateChange{Context: map[string]interface{}{"shift": value}}); err != nil {
		t.Fatalf("unable to turn the shift calendar: %v", err)
	}
}

func TestAClosedGateStandsTheMachineStillAndZeroesEveryDeclaredWrite(t *testing.T) {
	const envId = "env-schedule-closed"
	source := shortSchedule(
		domain.ScheduleState{Name: "idle", DurationSeconds: 1, Value: 400},
		domain.ScheduleState{Name: "running", DurationSeconds: 3600, Value: 9000,
			StateWrites: map[string]float64{"air_demand": 120}},
	)
	publisher := &fakePublisher{}
	runtime := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(gatedEnvironment(envId, source)), newFakeStates(), publisher)

	if !waitFor(6*time.Second, func() bool { return len(valuesOn(publisher, serviceRefOf(envId))) >= 2 }) {
		t.Fatal("a gated schedule has to publish while its gate is closed too")
	}
	for _, value := range valuesOn(publisher, serviceRefOf(envId)) {
		if value != 0.0 {
			t.Fatalf("a machine standing still has to read 0, got %v", valuesOn(publisher, serviceRefOf(envId)))
		}
	}
	if name := assetStateOf(t, runtime, envId, "programm"); name != domain.ScheduleClosedState {
		t.Errorf("expected the state to be named %q while the gate is closed, got %v", domain.ScheduleClosedState, name)
	}
	if air := assetStateOf(t, runtime, envId, "air_demand"); air != 0.0 {
		t.Errorf("a declared write of a state that is not running has to read 0, got %v", air)
	}
	//nothing is anchored while nothing has run: an anchor written here would be
	//a start instant of a programme that never started
	if _, known := runOf(t, runtime, envId, "ch-1"); known {
		t.Error("a schedule whose gate never opened must not write an anchor")
	}
}

func TestEveryRisingEdgeStartsTheProgrammeOverAtItsFirstState(t *testing.T) {
	const envId = "env-schedule-edge"
	source := shortSchedule(
		domain.ScheduleState{Name: "warmup", DurationSeconds: 1, Value: 10},
		domain.ScheduleState{Name: "running", DurationSeconds: 3600, Value: 20},
	)
	publisher := &fakePublisher{}
	runtime := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(gatedEnvironment(envId, source)), newFakeStates(), publisher)
	service := serviceRefOf(envId)

	countOf := func(want float64) func() int {
		return func() int {
			seen := 0
			for _, value := range valuesOn(publisher, service) {
				if value == want {
					seen++
				}
			}
			return seen
		}
	}
	warmups := countOf(10.0)

	setShift(t, runtime, envId, 1)
	if !waitFor(8*time.Second, func() bool { return warmups() >= 1 }) {
		t.Fatalf("the rising edge did not start the programme: %v", valuesOn(publisher, service))
	}
	if !waitFor(8*time.Second, func() bool { return countOf(20.0)() >= 1 }) {
		t.Fatalf("the programme did not reach its second state: %v", valuesOn(publisher, service))
	}
	opened, _ := runOf(t, runtime, envId, "ch-1")

	setShift(t, runtime, envId, 0)
	if !waitFor(8*time.Second, func() bool { return countOf(0.0)() >= 1 }) {
		t.Fatalf("the falling edge did not stand the machine still: %v", valuesOn(publisher, service))
	}
	seenBefore := warmups()

	setShift(t, runtime, envId, 1)
	if !waitFor(8*time.Second, func() bool { return warmups() > seenBefore }) {
		t.Fatalf("the second opening did not start the programme over: %v", valuesOn(publisher, service))
	}
	reopened, _ := runOf(t, runtime, envId, "ch-1")
	if reopened.StartUnix <= opened.StartUnix || reopened.CycleOffset != 0 {
		t.Errorf("the second opening has to be a new run, got %+v after %+v", reopened, opened)
	}
}

// A gated run rolls its anchor forward like any other cycling one - a gate that
// stays open would otherwise be walked from its rising edge for as long as it
// stays open - and what may not move while it does is the salt of the draws.
// PassUnix is written once, at the edge, and the shift that is running keeps
// the durations it started with.
func TestAGatedScheduleRollsItsAnchorButNotItsPass(t *testing.T) {
	const envId = "env-schedule-gated-anchor"
	source := shortSchedule(
		domain.ScheduleState{Name: "a", DurationSeconds: 1, Value: 1},
		domain.ScheduleState{Name: "b", DurationSeconds: 1, Value: 2},
	)
	publisher := &fakePublisher{}
	runtime := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(gatedEnvironment(envId, source)), newFakeStates(), publisher)

	setShift(t, runtime, envId, 1)
	opened := repo.ScheduleRun{}
	known := false
	if !waitFor(6*time.Second, func() bool { opened, known = runOf(t, runtime, envId, "ch-1"); return known && opened.Open }) {
		t.Fatal("the gate never opened")
	}
	if opened.PassUnix != opened.StartUnix {
		t.Errorf("a rising edge has to start the pass at its anchor, got %+v", opened)
	}
	//several cycles of two seconds each, which is where the roll-forward shows
	rolled := func() bool {
		run, _ := runOf(t, runtime, envId, "ch-1")
		return run.CycleOffset > 0
	}
	if !waitFor(12*time.Second, rolled) {
		run, _ := runOf(t, runtime, envId, "ch-1")
		t.Fatalf("the anchor of a gated run never rolled forward: %+v", run)
	}
	later, _ := runOf(t, runtime, envId, "ch-1")
	//two states of a second each: the anchor moves by whole cycles only
	if later.StartUnix-opened.StartUnix != 2*later.CycleOffset {
		t.Errorf("anchor and cycle counter disagree: %+v after %+v", later, opened)
	}
	if later.PassUnix != opened.PassUnix {
		t.Errorf("the pass salt moved with the anchor, so the running shift was redrawn: %+v after %+v", later, opened)
	}
}

// A run stored before the pass salt was split off the anchor. Reading the salt
// from StartUnix as a fallback would follow the roll-forward, so the salt is
// adopted once, on the next evaluation, and stands from there.
func TestAnOpenRunWithoutAPassSaltAdoptsItsAnchorOnce(t *testing.T) {
	const envId = "env-schedule-legacy-pass"
	source := shortSchedule(
		domain.ScheduleState{Name: "a", DurationSeconds: 1, Value: 1},
		domain.ScheduleState{Name: "b", DurationSeconds: 1, Value: 2},
	)
	source.Gate = &domain.ScheduleGate{ContextKey: "shift"}
	runtime := newRuntime(testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), nil, &fakePublisher{})
	env := &environment{id: envId, state: repo.RuntimeState{
		Context:      map[string]interface{}{"shift": 1.0},
		ScheduleRuns: map[string]repo.ScheduleRun{"ch-1": {StartUnix: scheduleT.Unix(), Open: true}},
	}}
	binding := channelBinding{channel: scheduleChannel("ch-1", "service", 1, source)}
	//no timeline in this document, so the gate reads the inline threshold
	gen := newGeneration(domain.Environment{Id: envId}, nil)

	run, open := runtime.scheduleRun(env, gen, binding, source, scheduleT.Add(time.Hour))
	if !open {
		t.Fatal("the gate is open, so the run has to be too")
	}
	if run.PassUnix != scheduleT.Unix() {
		t.Errorf("the stored anchor is the instant the pass began and has to become its salt, got %+v", run)
	}
	if run.StartUnix != scheduleT.Unix() || run.CycleOffset != 0 {
		t.Errorf("adopting the salt must not restart the programme, got %+v", run)
	}
	//and once the anchor has rolled, the salt stays where it was adopted
	env.state.ScheduleRuns["ch-1"] = repo.ScheduleRun{
		StartUnix: scheduleT.Unix() + 3600, CycleOffset: 1800, PassUnix: scheduleT.Unix(), Open: true}
	rolled, _ := runtime.scheduleRun(env, gen, binding, source, scheduleT.Add(2*time.Hour))
	if rolled.PassUnix != scheduleT.Unix() {
		t.Errorf("the adopted salt was overwritten by the rolled anchor: %+v", rolled)
	}
}

// The union: every key any state of the schedule declares is written on every
// evaluation. Without it the air demand of a machine that was running would
// still stand while it idles, and nothing in the document would say so.
func TestAStateWriteOfAnotherStateIsZeroedRatherThanLeftStanding(t *testing.T) {
	const envId = "env-schedule-union"
	source := shortSchedule(
		domain.ScheduleState{Name: "idle", DurationSeconds: 1, Value: 400},
		domain.ScheduleState{Name: "running", DurationSeconds: 1, Value: 9000,
			StateWrites: map[string]float64{"air_demand": 120}},
	)
	publisher := &fakePublisher{}
	runtime := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(testEnvironment(envId,
		scheduleChannel("ch-1", serviceRefOf(envId), 1, source))), newFakeStates(), publisher)

	idle, running := false, false
	check := func() bool {
		snapshot, err := runtime.Snapshot(envId)
		if err != nil {
			t.Fatalf("unable to read the state: %v", err)
		}
		//name and value are read out of one snapshot, so they are the pair one
		//evaluation wrote and not two moments stitched together
		states := snapshot.State.Assets[testAssetId]
		switch states["programm"] {
		case "idle":
			idle = true
			if states["air_demand"] != 0.0 {
				t.Fatalf("the air demand of the running state stood while the machine idled: %v", states["air_demand"])
			}
		case "running":
			running = true
			if states["air_demand"] != 120.0 {
				t.Fatalf("the running state did not declare its air demand: %v", states["air_demand"])
			}
		}
		return idle && running
	}
	if !waitFor(10*time.Second, check) {
		t.Errorf("expected to see both states, idle %v running %v", idle, running)
	}
}

// A schedule standing in one state writes the same name and the same numbers on
// every evaluation. That must not mark the environment dirty, or a site of
// declared machines would have its whole state document rewritten on every
// flush interval for nothing.
func TestAScheduleStandingStillDoesNotRewriteTheState(t *testing.T) {
	const envId = "env-schedule-quiet"
	source := shortSchedule(domain.ScheduleState{Name: "running", DurationSeconds: 3600, Value: 9000,
		StateWrites: map[string]float64{"air_demand": 120}})
	publisher := &fakePublisher{}
	states := newFakeStates()
	startRuntime(t, testConfig(20*time.Millisecond), newFakeEnvironments(testEnvironment(envId,
		scheduleChannel("ch-1", serviceRefOf(envId), 1, source))), states, publisher)

	if !waitFor(6*time.Second, func() bool { return len(states.savedFor(envId)) >= 1 }) {
		t.Fatal("the anchor of the schedule was never written")
	}
	//more than a hundred flush intervals and several evaluations later, nothing
	//new may have been written
	settled := len(states.savedFor(envId))
	time.Sleep(3 * time.Second)
	if grown := len(states.savedFor(envId)); grown > settled {
		t.Errorf("a schedule standing still wrote the state %d more times", grown-settled)
	}
}

// The prune, for the reason the value cache and the last published values have
// one: an anchor of a channel that no longer runs a schedule is a position
// nothing reads, written out on every flush, and it would come back as the
// position of a programme that has not run for weeks if the source were ever
// switched back.
func TestAReloadWithoutTheScheduleDropsItsAnchor(t *testing.T) {
	const envId = "env-schedule-prune"
	source := shortSchedule(domain.ScheduleState{Name: "running", DurationSeconds: 3600, Value: 9000})
	envs := newFakeEnvironments(testEnvironment(envId, scheduleChannel("ch-1", serviceRefOf(envId), 1, source)))
	publisher := &fakePublisher{}
	runtime := startRuntime(t, testConfig(time.Hour), envs, newFakeStates(), publisher)

	if !waitFor(6*time.Second, func() bool { _, known := runOf(t, runtime, envId, "ch-1"); return known }) {
		t.Fatal("setup: the schedule never wrote an anchor")
	}

	changed := testEnvironment(envId, profileChannel("ch-1", serviceRefOf(envId), 1, flatProfile(230, 0)))
	if err := domain.Validate(changed); err != nil {
		t.Fatalf("the changed document has to be a legal one: %v", err)
	}
	if _, err := envs.Put(context.Background(), changed); err != nil {
		t.Fatalf("unable to store the changed document: %v", err)
	}
	runtime.Reload(envId)

	if _, known := runOf(t, runtime, envId, "ch-1"); known {
		t.Error("the anchor of a channel that no longer runs a schedule was kept")
	}
}

// What the state key and the state writes are for: another channel of the same
// asset reads them, and the schedule is what a formula stands on.
func TestAFormulaReadsAValueAScheduleDeclares(t *testing.T) {
	const envId = "env-schedule-formula"
	source := shortSchedule(
		domain.ScheduleState{Name: "idle", DurationSeconds: 1, Value: 400},
		domain.ScheduleState{Name: "running", DurationSeconds: 2, Value: 9000,
			StateWrites: map[string]float64{"air_demand": 120}},
	)
	derived := domain.Channel{
		Id: "ch-air", Name: "Druckluft", Direction: domain.Sensor,
		ExternalRef: serviceRefOf(envId) + "-air", IntervalSeconds: 1,
		Source: domain.Source{Kind: domain.SourceFormula, Formula: &domain.FormulaSource{
			Expression: "air * 2", Inputs: map[string]string{"air": "asset.air_demand"}}},
	}
	env := testEnvironment(envId, scheduleChannel("ch-1", serviceRefOf(envId), 1, source), derived)
	if err := domain.Validate(env); err != nil {
		t.Fatalf("the document has to be a legal one: %v", err)
	}
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(10*time.Second, func() bool { return sawValue(publisher, serviceRefOf(envId)+"-air", 240.0) }) {
		t.Fatalf("the formula never read the air demand the schedule declares: %v",
			valuesOn(publisher, serviceRefOf(envId)+"-air"))
	}
	//and it reads 0 while the machine idles rather than the value of the state
	//that is not running
	if !waitFor(10*time.Second, func() bool { return sawValue(publisher, serviceRefOf(envId)+"-air", 0.0) }) {
		t.Fatalf("the union of the state writes never reached the formula: %v",
			valuesOn(publisher, serviceRefOf(envId)+"-air"))
	}
}

// The comparison in front of every state write. It is what keeps a machine
// standing in one state from marking its environment dirty on every
// evaluation - and it has to survive a key that already holds something a bare
// == would panic on, which is any map or slice an initial_states brought along.
func TestWritingAStateValueThatDidNotChangeIsNotAChange(t *testing.T) {
	states := map[string]interface{}{}
	if !setStateValue(states, "programm", "idle") {
		t.Error("the first write of a key is a change")
	}
	if setStateValue(states, "programm", "idle") {
		t.Error("writing the same name again is not a change")
	}
	if !setStateValue(states, "programm", "running") {
		t.Error("a different name is a change")
	}
	if !setStateValue(states, "air_demand", 120.0) {
		t.Error("the first write of a number is a change")
	}
	if setStateValue(states, "air_demand", 120.0) {
		t.Error("writing the same number again is not a change")
	}
	//a document decoded from bson carries whole numbers as ints, and rewriting
	//those on every tick would be the churn this comparison exists to avoid
	states["air_demand"] = int64(120)
	if setStateValue(states, "air_demand", 120.0) {
		t.Error("an int state carrying the same number is not a change")
	}
	//and a key an initial_states filled with something structured: a change,
	//and above all not a panic
	states["programm"] = map[string]interface{}{"nested": 1}
	if !setStateValue(states, "programm", "idle") {
		t.Error("overwriting a structured value with a name is a change")
	}
	//a type the schedule never writes compares as different rather than equal
	if sameStateValue("idle", true) {
		t.Error("a value that is neither a name nor a number must not compare equal")
	}
}

// The same discipline for the anchor: a run that is written back unchanged is
// not a change either, or every schedule of a site would rewrite its whole
// state document on every flush interval.
func TestStoringAnUnchangedRunDoesNotDirtyTheEnvironment(t *testing.T) {
	runtime := newRuntime(testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), nil, &fakePublisher{})
	env := &environment{id: "env-1"}
	run := repo.ScheduleRun{StartUnix: scheduleT.Unix(), Open: true}

	runtime.storeScheduleRun(env, "ch-1", run)
	if !env.dirty {
		t.Error("a run that was not there yet is a change")
	}
	env.dirty = false
	runtime.storeScheduleRun(env, "ch-1", run)
	if env.dirty {
		t.Error("writing the same run again is not a change")
	}
	run.CycleOffset++
	runtime.storeScheduleRun(env, "ch-1", run)
	if !env.dirty {
		t.Error("a rolled anchor is a change and has to be flushed")
	}
}

// A schedule without states cannot be executed and is dropped by the
// generation. The pure walk still has to answer rather than index into nothing.
func TestAScheduleWithoutStatesWalksNowhere(t *testing.T) {
	got := scheduleAt(domain.ScheduleSource{StateKey: "programm"}, scheduleSeed, "ch", runAt(scheduleT), scheduleT.Unix()+50)
	if got != (schedulePosition{}) {
		t.Errorf("expected an empty position, got %+v", got)
	}
}

// A schedule stands where its anchor and its gate put it, and neither of those
// exists for a past moment.
func TestAScheduleIsNotBackfilled(t *testing.T) {
	channel := scheduleChannel("ch-1", "service", 60, shortSchedule(
		domain.ScheduleState{Name: "idle", DurationSeconds: 60, Value: 1}))
	reason := backfillSkipsByDefinition(channel)
	if reason == "" {
		t.Fatal("a schedule must not be backfilled")
	}
	if !strings.Contains(reason, "anchor") {
		t.Errorf("the reason has to say why, got %q", reason)
	}
}
