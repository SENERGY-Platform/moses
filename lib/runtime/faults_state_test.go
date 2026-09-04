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
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// What the fault does to a channel publishing on change, what the rest of the
// simulation sees while it happens, and what of it is persisted.

// ---------------------------------------------------------------------------
// the change trigger
// ---------------------------------------------------------------------------

// covFixture is a runtime and an environment carrying one channel, ready for a
// direct call into the gate.
func covFixture(t *testing.T, channel domain.Channel) (*Runtime, *environment, channelBinding, *fakePublisher) {
	t.Helper()
	def := testEnvironment("env-fault-cov", channel)
	def.Seed = faultParitySeed
	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, publisher)
	gen := newGeneration(def, nil)
	if len(gen.sensors) != 1 {
		t.Fatalf("the fixture is meant to carry one ticking channel, got %d", len(gen.sensors))
	}
	env := &environment{id: def.Id, gen: gen, state: repo.RuntimeState{EnvironmentId: def.Id}}
	return rt, env, gen.sensors[0], publisher
}

// An outage must leave the comparison base exactly where it was: it suppressed a
// reading, it did not publish one, and a base moved by a reading nobody received
// would silence the channel afterwards.
func TestAnOutageLeavesTheComparisonBaseUntouched(t *testing.T) {
	channel := profileChannel("ch-out", serviceRefOf("env-fault-cov"), 600, flatProfile(230, 0))
	channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 1, EvaluateIntervalSeconds: 60}
	channel.Faults = []domain.Fault{{Kind: domain.FaultOutage, From: faultOutageFrom, To: faultOutageTo}}
	rt, env, binding, _ := covFixture(t, channel)

	env.state.LastPublished = map[string]repo.PublishedValue{
		binding.channel.Id: {Value: 100, AtUnix: faultOutageFrom.Add(-time.Minute).Unix()},
	}
	sent := 0
	send := func(value interface{}) bool { sent++; return true }

	env.mux.Lock()
	defer env.mux.Unlock()
	//a value far past the threshold, and the heartbeat on top of it
	for _, forced := range []bool{false, true} {
		if rt.covGate(env, binding, &faultRun{}, 900.0, forced, faultOutageFrom, send) {
			t.Fatalf("forced=%v: the outage has to suppress the publish", forced)
		}
	}
	if sent != 0 {
		t.Fatalf("nothing may reach the platform during an outage, got %d sends", sent)
	}
	if got := env.state.LastPublished[binding.channel.Id]; got.Value != 100 {
		t.Fatalf("the comparison base moved to %v during an outage", got.Value)
	}
}

// A frozen reading looks unchanged to the trigger, so the channel falls back to
// its heartbeat cadence - realistic, and the reason the fault is applied ahead of
// the threshold rather than behind it.
func TestAFrozenChannelFallsBackToItsHeartbeatCadence(t *testing.T) {
	const id = "env-fault-freeze"
	from := faultParityFrom
	to := from.Add(2 * time.Hour)
	freezeFrom := from.Add(30 * time.Minute)
	freezeTo := from.Add(90 * time.Minute)

	//a spread wide enough that every evaluation would otherwise cross the
	//threshold, so the freeze is what makes the channel quiet
	channel := profileChannel("ch-freeze", serviceRefOf(id), 600,
		domain.ProfileSource{Base: 230, SpreadPercent: 20})
	channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 1, EvaluateIntervalSeconds: 60}
	channel.Faults = []domain.Fault{{Kind: domain.FaultFrozen, From: freezeFrom, To: freezeTo}}
	def := testEnvironment(id, channel)
	def.Seed = faultParitySeed

	publisher := faultParityRun(t, def, from, to)
	inside, outside := 0, 0
	values := map[int64]float64{}
	for _, event := range publisher.backfilled(serviceRefOf(id)) {
		number, _ := event.value.(float64)
		values[event.at.Unix()] = number
		switch {
		case !event.at.Before(freezeFrom) && event.at.Before(freezeTo):
			inside++
		default:
			outside++
		}
	}
	//60 minutes of freeze at a 600 second heartbeat: the first frozen reading
	//still counts as a change, the rest is heartbeats
	if inside > 7 {
		t.Errorf("a frozen channel is meant to fall back to its heartbeat, got %d publishes inside the window", inside)
	}
	if inside == 0 {
		t.Error("a frozen channel still publishes on its heartbeat")
	}
	if outside <= inside {
		t.Errorf("outside the freeze the channel publishes far more often, got %d against %d", outside, inside)
	}
	//and every reading inside the window is the same number
	var held float64
	for at, value := range values {
		if at < freezeFrom.Unix() || at >= freezeTo.Unix() {
			continue
		}
		if held == 0 {
			held = value
			continue
		}
		if value != held {
			t.Fatalf("a frozen channel repeats one value, got %v next to %v", value, held)
		}
	}
}

// A spike publishes twice: the outlier, and the return to normal - which is what
// the hardware does, and what a consumer has to be able to see.
func TestASpikePublishesTheOutlierAndTheReturn(t *testing.T) {
	const id = "env-fault-spike"
	from := faultParityFrom
	to := from.Add(time.Hour)
	spikeFrom := from.Add(30 * time.Minute)
	spikeTo := spikeFrom.Add(60 * time.Second)

	//flat, so nothing but the heartbeat and the spike can publish
	channel := profileChannel("ch-spike", serviceRefOf(id), 600, flatProfile(230, 0))
	channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 10, EvaluateIntervalSeconds: 60}
	channel.Faults = []domain.Fault{{Kind: domain.FaultSpike, From: spikeFrom, To: spikeTo, Factor: 12}}
	def := testEnvironment(id, channel)
	def.Seed = faultParitySeed

	published := map[int64]float64{}
	for _, event := range faultParityRun(t, def, from, to).backfilled(serviceRefOf(id)) {
		number, _ := event.value.(float64)
		published[event.at.Unix()-from.Unix()] = number
	}
	if got, sent := published[1800]; !sent || got != 2760 {
		t.Errorf("the outlier goes out at 1800 seconds, got %v sent=%v", got, sent)
	}
	if got, sent := published[1860]; !sent || got != 230 {
		t.Errorf("the return to normal goes out at 1860 seconds, got %v sent=%v", got, sent)
	}
}

// ---------------------------------------------------------------------------
// the live runners
// ---------------------------------------------------------------------------

// The two ticker shapes have their own hook each - the plain ticker sends inside
// the dispatch, the publish half of a split channel outside it and has to take
// the environment mutex itself - and neither is reached by a reconstruction, so
// they are asserted against the running runtime.
func TestBothLiveRunnerShapesSuppressAnOutage(t *testing.T) {
	const id = "env-fault-live"
	//a window that covers every instant the clock can be at while this runs
	always := domain.Fault{Kind: domain.FaultOutage,
		From: time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2262, 1, 1, 0, 0, 0, 0, time.UTC)}

	plain := profileChannel("ch-live-plain", serviceRefOf(id)+"-plain", 1, flatProfile(230, 0))
	plain.Faults = []domain.Fault{always}
	split := scriptChannel("ch-live-split", domain.Sensor, 1, serviceRefOf(id)+"-split", "moses.service.send(42);")
	split.Source.IntervalSeconds = 1
	split.Faults = []domain.Fault{always}
	control := profileChannel("ch-live-control", serviceRefOf(id)+"-control", 1, flatProfile(230, 0))

	def := testEnvironment(id, plain, split, control)
	if err := domain.Validate(def); err != nil {
		t.Fatalf("the fixture has to be a storable document: %v", err)
	}
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), publisher)

	forService := func(serviceRef string) int {
		count := 0
		for _, event := range publisher.all() {
			if event.serviceRef == serviceRef {
				count++
			}
		}
		return count
	}
	//the control channel is what says the runtime got as far as publishing at all
	if !waitFor(6*time.Second, func() bool { return forService(serviceRefOf(id)+"-control") >= 3 }) {
		t.Fatalf("the undisturbed channel did not publish, got %d", forService(serviceRefOf(id)+"-control"))
	}
	if got := forService(serviceRefOf(id) + "-plain"); got != 0 {
		t.Errorf("the plain ticker published %d readings while its outage was running", got)
	}
	if got := forService(serviceRefOf(id) + "-split"); got != 0 {
		t.Errorf("the publish half of the split channel published %d readings while its outage was running", got)
	}
}

// ---------------------------------------------------------------------------
// the ground truth
// ---------------------------------------------------------------------------

// The whole point of injecting the fault into the measurement: everything inside
// the simulation keeps the undisturbed value, so a formula over the faulted
// channel is the ground truth without a line of code for it.
func TestAFormulaOverAFaultedChannelPublishesTheUndisturbedValue(t *testing.T) {
	const id = "env-fault-truth"
	from := faultParityFrom
	to := from.Add(2 * time.Hour)
	spikeFrom := from.Add(30 * time.Minute)
	spikeTo := from.Add(60 * time.Minute)

	meter := profileChannel("ch-meter", serviceRefOf(id)+"-meter", 600,
		domain.ProfileSource{Base: 120, SpreadPercent: 20, Cumulative: true})
	meter.Faults = []domain.Fault{
		{Kind: domain.FaultSpike, From: spikeFrom, To: spikeTo, Factor: 12},
	}
	truth := formulaChannel(id, "x", map[string]string{"x": "channel.ch-meter"})
	truth.IntervalSeconds = 600
	truth.ExternalRef = serviceRefOf(id) + "-truth"
	def := testEnvironment(id, meter, truth)
	def.Seed = faultParitySeed
	if err := domain.Validate(def); err != nil {
		t.Fatalf("the fixture has to be a storable document: %v", err)
	}

	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, publisher)
	gen := newGeneration(def, nil)
	env := &environment{id: def.Id, gen: gen, state: repo.RuntimeState{EnvironmentId: def.Id}}
	env.resetForHistory()
	env.seed(gen, from)
	if _, err := rt.runHistory(t.Context(), env, gen, from, to, keepTheWindow, nil); err != nil {
		t.Fatalf("the history run failed: %v", err)
	}

	//the undisturbed counter, computed from the pure function alone
	counter := float64(0)
	want := []float64{}
	for i := int64(0); i < backfillTicks(600, from, to); i++ {
		at := from.Add(time.Duration(i*600) * time.Second).In(time.Local)
		counter += profileValue(domain.ProfileSource{Base: 120, SpreadPercent: 20, Cumulative: true},
			def.Seed, "ch-meter", 600, at) * 600 / 3600
		want = append(want, counter)
	}

	truthValues := historyValues(publisher, serviceRefOf(id)+"-truth")
	if len(truthValues) != len(want) {
		t.Fatalf("the formula published %d readings, expected %d", len(truthValues), len(want))
	}
	for i := range want {
		if truthValues[i] != want[i] {
			t.Fatalf("reading %d: the formula published %v, the undisturbed value is %v", i, truthValues[i], want[i])
		}
	}

	//and the meter itself really was disturbed
	meterValues := historyValues(publisher, serviceRefOf(id)+"-meter")
	if equalReadings(meterValues, want) {
		t.Fatal("the fixture disturbed nothing, so the comparison proves nothing")
	}

	//the asset counter is the undisturbed one too: a fault never flows back into
	//the state the simulation carries forward
	env.mux.Lock()
	stored, _ := asFloat(env.state.Assets[testAssetId]["ch-meter"])
	cached := env.lastValues["ch-meter"]
	env.mux.Unlock()
	last := want[len(want)-1]
	if math.Abs(stored-last) > 1e-9 {
		t.Errorf("the asset counter reads %v, the undisturbed total is %v", stored, last)
	}
	if math.Abs(cached-last) > 1e-9 {
		t.Errorf("the value cache reads %v, the undisturbed total is %v", cached, last)
	}
}

// ---------------------------------------------------------------------------
// the persisted offset
// ---------------------------------------------------------------------------

// exchangeChannel is a cumulative meter that is exchanged at faultExchangeAt.
func exchangeChannel(envId string) domain.Channel {
	channel := profileChannel("ch-exchange", serviceRefOf(envId), 600,
		domain.ProfileSource{Base: 120, Cumulative: true})
	channel.Faults = []domain.Fault{{Kind: domain.FaultMeterExchange, From: faultExchangeAt, ResetTo: 0}}
	return channel
}

// A history run leaves the offset it captured in the state it hands over, so the
// live channel that follows continues on the new register instead of jumping back
// to the old reading.
func TestAHistoryRunHandsOverTheCapturedMeterOffset(t *testing.T) {
	const id = "env-fault-handover"
	from := faultParityFrom
	to := from.Add(4 * time.Hour)
	def := testEnvironment(id, exchangeChannel(id))
	def.Seed = faultParitySeed

	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, publisher)
	gen := newGeneration(def, nil)
	env := &environment{id: def.Id, gen: gen, state: repo.RuntimeState{EnvironmentId: def.Id}}
	env.resetForHistory()
	env.seed(gen, from)
	if _, err := rt.runHistory(t.Context(), env, gen, from, to, keepTheWindow, nil); err != nil {
		t.Fatalf("the history run failed: %v", err)
	}

	env.mux.Lock()
	stored := env.snapshot()
	env.mux.Unlock()
	if len(stored.MeterExchanges) != 1 {
		t.Fatalf("the run has to hand over exactly one captured offset, got %v", stored.MeterExchanges)
	}
	key := meterExchangeKey("ch-exchange", faultExchangeAt.Unix())
	offset, known := stored.MeterExchanges[key]
	if !known {
		t.Fatalf("the offset is stored under %q, got %v", key, stored.MeterExchanges)
	}
	if offset >= 0 {
		t.Errorf("the offset takes the old reading off the new register, got %v", offset)
	}

	//and the offset survives a restart: an environment loaded from that state
	//keeps counting on the new register rather than capturing a second offset
	restarted := &environment{id: id, gen: gen, state: stored}
	binding := gen.sensors[0]
	restarted.mux.Lock()
	value, send := rt.faulted(restarted, binding, &faultRun{}, 5000.0, to.Add(time.Hour))
	restarted.mux.Unlock()
	if !send {
		t.Fatal("nothing suppresses this reading")
	}
	if got, _ := asFloat(value); got != 5000+offset {
		t.Errorf("after a restart the register reads %v, expected %v", got, 5000+offset)
	}
	if len(restarted.state.MeterExchanges) != 1 {
		t.Errorf("a restart must not capture a second offset, got %v", restarted.state.MeterExchanges)
	}
}

// The backfill keeps its own offsets: a reconstructed window must not move the
// register the live simulation counts on.
func TestABackfillNeverTouchesTheLiveMeterOffsets(t *testing.T) {
	const id = "env-fault-jobstate"
	from := faultParityFrom
	to := from.Add(4 * time.Hour)
	def := testEnvironment(id, exchangeChannel(id))
	def.Seed = faultParitySeed

	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, publisher)
	gen := newGeneration(def, nil)
	live := map[string]float64{"sentinel": 17}
	env := &environment{id: id, gen: gen, state: repo.RuntimeState{EnvironmentId: id, MeterExchanges: live}}

	job := &backfillJob{done: make(chan struct{}), status: BackfillStatus{EnvironmentId: id}}
	//one pool for every channel, as the job has it
	pool := testPublishPool(t, rt)
	for _, channel := range backfillChannels(def) {
		status := BackfillChannelStatus{ChannelId: channel.channel.Id}
		rt.runBackfillChannel(context.Background(), pool, job, gen, channel, nil, from, to, &status)
		if status.Published == 0 {
			t.Fatal("the job is meant to publish something")
		}
	}
	if len(env.state.MeterExchanges) != 1 || env.state.MeterExchanges["sentinel"] != 17 {
		t.Fatalf("the job wrote into the live offsets: %v", env.state.MeterExchanges)
	}
	if env.dirty {
		t.Error("the job must not mark the live state dirty")
	}
	//and the job did reconstruct the exchange, so the assertion above is about a
	//job that had an offset to capture
	values := historyValues(publisher, serviceRefOf(id))
	restarted := false
	for i := 1; i < len(values); i++ {
		if values[i] < values[i-1] {
			restarted = true
		}
	}
	if !restarted {
		t.Fatal("the reconstructed register is meant to restart halfway through the window")
	}
}

// An offset belongs to one exchange of one channel. Once that fault is gone from
// the document nothing reads it again, so it is pruned rather than written out on
// every flush forever.
func TestACapturedOffsetIsPrunedWhenTheFaultIsGone(t *testing.T) {
	const id = "env-fault-prune"
	def := testEnvironment(id, exchangeChannel(id))
	def.Seed = faultParitySeed
	gen := newGeneration(def, nil)
	kept := meterExchangeKey("ch-exchange", faultExchangeAt.Unix())

	env := &environment{id: id, state: repo.RuntimeState{EnvironmentId: id,
		MeterExchanges: map[string]float64{kept: -42, "stale-key": 7}}}
	env.carryLastValues(gen)
	if _, known := env.state.MeterExchanges[kept]; !known {
		t.Fatalf("the offset of a fault the document still carries is kept, got %v", env.state.MeterExchanges)
	}
	if _, known := env.state.MeterExchanges["stale-key"]; known {
		t.Fatalf("an offset nothing reads is dropped, got %v", env.state.MeterExchanges)
	}
	if !env.dirty {
		t.Error("dropping a stored entry is a change and has to be flushed")
	}

	//the fault removed from the document: now the entry that was kept goes too
	without := testEnvironment(id, profileChannel("ch-exchange", serviceRefOf(id), 600,
		domain.ProfileSource{Base: 120, Cumulative: true}))
	without.Seed = faultParitySeed
	env.dirty = false
	env.carryLastValues(newGeneration(without, nil))
	if len(env.state.MeterExchanges) != 0 {
		t.Fatalf("an offset of a fault that is gone is dropped, got %v", env.state.MeterExchanges)
	}
	if !env.dirty {
		t.Error("dropping the last entry is a change too")
	}
}

// Deleting an unrelated fault ahead of the exchange must not disturb it. With the
// offset keyed by the fault's position in the list it did: every index behind the
// deleted entry shifted, the prune read the stored offset as belonging to nothing,
// and the very next reading restarted the register at reset_to - a backwards step
// in a cumulative counter, which is exactly the signal "the meter was exchanged",
// raised by an edit that touched something else entirely.
func TestDeletingAFaultAheadOfAnExchangeLeavesTheRegisterAlone(t *testing.T) {
	const id = "env-fault-shift"
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), nil, &fakePublisher{})

	before := profileChannel("ch-exchange", serviceRefOf(id), 600,
		domain.ProfileSource{Base: 120, Cumulative: true})
	before.Faults = []domain.Fault{
		{Kind: domain.FaultOutage, From: faultParityFrom, To: faultParityFrom.Add(time.Hour)},
		{Kind: domain.FaultMeterExchange, From: faultExchangeAt, ResetTo: 0},
	}
	def := testEnvironment(id, before)
	def.Seed = faultParitySeed
	if err := domain.Validate(def); err != nil {
		t.Fatalf("the fixture has to be a storable document: %v", err)
	}

	//the exchange happens and its offset is captured
	gen := newGeneration(def, nil)
	env := &environment{id: id, gen: gen, state: repo.RuntimeState{EnvironmentId: id}}
	binding := gen.sensors[0]
	after := faultExchangeAt.Add(time.Hour)
	env.mux.Lock()
	rt.faulted(env, binding, &faultRun{}, 5000.0, faultExchangeAt)
	reading, _ := rt.faulted(env, binding, &faultRun{}, 5100.0, after)
	env.mux.Unlock()
	running, _ := asFloat(reading)
	if running != 100 {
		t.Fatalf("the new register is meant to read 100 before the edit, got %v", running)
	}
	if len(env.state.MeterExchanges) != 1 {
		t.Fatalf("one offset is captured, got %v", env.state.MeterExchanges)
	}

	//the outage ahead of it is deleted, which shifts the exchange from index 1 to 0
	edited := before
	edited.Faults = []domain.Fault{before.Faults[1]}
	next := testEnvironment(id, edited)
	next.Seed = faultParitySeed
	reloaded := newGeneration(next, nil)
	env.carryLastValues(reloaded)

	if len(env.state.MeterExchanges) != 1 {
		t.Fatalf("the offset belongs to an exchange the document still carries and must survive the edit, got %v", env.state.MeterExchanges)
	}
	env.mux.Lock()
	reading, _ = rt.faulted(env, reloaded.sensors[0], &faultRun{}, 5200.0, after.Add(time.Hour))
	env.mux.Unlock()
	got, _ := asFloat(reading)
	//200, not 0: the register kept counting instead of restarting
	if got != 200 {
		t.Fatalf("after deleting an unrelated fault the register reads %v; a jump back to reset_to would be a phantom meter exchange", got)
	}
}

// Redating the exchange is a different exchange: the old offset was captured
// against another instant and must not be carried into the new one.
func TestRedatingAnExchangeDropsTheOldOffset(t *testing.T) {
	const id = "env-fault-redate"
	channel := exchangeChannel(id)
	channel.Faults[0].From = faultExchangeAt.Add(time.Hour)
	def := testEnvironment(id, channel)
	def.Seed = faultParitySeed

	env := &environment{id: id, state: repo.RuntimeState{EnvironmentId: id,
		MeterExchanges: map[string]float64{meterExchangeKey("ch-exchange", faultExchangeAt.Unix()): -42}}}
	env.carryLastValues(newGeneration(def, nil))
	if len(env.state.MeterExchanges) != 0 {
		t.Fatalf("the offset of the old date is dropped, got %v", env.state.MeterExchanges)
	}
}
