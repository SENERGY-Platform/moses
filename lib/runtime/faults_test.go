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
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

// The core of the feature is pure: which fault covers which instant follows from
// the seed, the channel id and the clock alone, so the live runtime, a backfill
// and a history run reach the same answer without talking to each other. These
// tests are about that function; the parity test is about the three paths.

const (
	faultSeed    = int64(4711)
	faultChannel = "ch-fault"
	faultStep    = int64(60)
)

var (
	faultBegin = time.Date(2026, 7, 1, 6, 0, 0, 0, time.UTC)
	faultEnd   = time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
)

// faultedChannel is a cumulative profile channel, the one shape every fault kind
// including the meter exchange applies to.
func faultedChannel(faults ...domain.Fault) domain.Channel {
	return domain.Channel{
		Id: faultChannel, Name: faultChannel, Direction: domain.Sensor,
		ExternalRef: "urn:infai:ses:service:fault", IntervalSeconds: faultStep,
		Source: domain.Source{Kind: domain.SourceProfile,
			Profile: &domain.ProfileSource{Base: 10, Cumulative: true}},
		Faults: faults,
	}
}

func resolvedOf(t *testing.T, faults ...domain.Fault) channelFaults {
	t.Helper()
	resolved, reasons := newChannelFaults(faultSeed, faultedChannel(faults...), faultStep)
	if len(reasons) > 0 {
		t.Fatalf("the fixture is meant to resolve, got %v", reasons)
	}
	if len(resolved.list) != len(faults) {
		t.Fatalf("expected %d resolved faults, got %d", len(faults), len(resolved.list))
	}
	return resolved
}

// reading applies the faults to one value with fresh memory on every call, which
// is what a test of a single instant wants.
func reading(faults channelFaults, value float64, at time.Time) (float64, bool) {
	return faultedReading(faults, &faultRun{}, map[string]float64{}, value, at)
}

// ---------------------------------------------------------------------------
// the window
// ---------------------------------------------------------------------------

// "from this instant on, until that one" has to include the start and exclude the
// end, so two windows meeting at one instant do not both cover it.
func TestAFaultWindowIncludesItsStartAndExcludesItsEnd(t *testing.T) {
	faults := resolvedOf(t, domain.Fault{Kind: domain.FaultOutage, From: faultBegin, To: faultEnd})
	for _, testCase := range []struct {
		name string
		at   time.Time
		send bool
	}{
		{"one second before the start", faultBegin.Add(-time.Second), true},
		{"the start itself", faultBegin, false},
		{"in the middle", faultBegin.Add(time.Hour), false},
		{"one second before the end", faultEnd.Add(-time.Second), false},
		{"the end itself", faultEnd, true},
	} {
		if _, send := reading(faults, 100, testCase.at); send != testCase.send {
			t.Errorf("%s: send %v, expected %v", testCase.name, send, testCase.send)
		}
	}
}

// ---------------------------------------------------------------------------
// the drawn occurrence
// ---------------------------------------------------------------------------

// beganSlots collects the steps a rate fault starts an occurrence in, using the
// draw itself rather than the search - so the search below is compared against an
// answer that does not come from it.
func beganSlots(faults channelFaults, fault resolvedFault, first int64, last int64) map[int64]bool {
	result := map[int64]bool{}
	for slot := first; slot <= last; slot++ {
		if faultBegan(faults.seed, faults.channelId, fault.index, slot, fault.probability) {
			result[slot] = true
		}
	}
	return result
}

// The same seed has to draw the same occurrences, or a reconstruction of a window
// would carry different defects than the live series did; a different seed has to
// draw others, or the seed would not be doing anything.
func TestTheSameSeedDrawsTheSameOccurrencesAndAnotherSeedDrawsOthers(t *testing.T) {
	fault := domain.Fault{Kind: domain.FaultOutage, PerHour: 6, DurationSeconds: 300}
	first, _ := newChannelFaults(faultSeed, faultedChannel(fault), faultStep)
	again, _ := newChannelFaults(faultSeed, faultedChannel(fault), faultStep)
	other, _ := newChannelFaults(faultSeed+1, faultedChannel(fault), faultStep)

	base := faultBegin
	same, different := 0, 0
	for i := 0; i < 2000; i++ {
		at := base.Add(time.Duration(int64(i)*faultStep) * time.Second)
		_, one := reading(first, 100, at)
		_, two := reading(again, 100, at)
		_, three := reading(other, 100, at)
		if one != two {
			t.Fatalf("the same seed produced two different answers at %v", at)
		}
		if one {
			same++
		}
		if one != three {
			different++
		}
	}
	if same == 2000 {
		t.Fatal("the fixture never suppressed anything, so the comparison proves nothing")
	}
	if different == 0 {
		t.Fatal("a different seed drew exactly the same occurrences")
	}
}

// A running occurrence is found again from every step it still covers, which is
// what makes the rate reproducible without any state. The expectation is built
// from the draw directly.
func TestARunningDrawnOccurrenceIsFoundFromEveryStepItCovers(t *testing.T) {
	//four steps long, so an occurrence spans several of them
	const duration = 4 * faultStep
	faults := resolvedOf(t, domain.Fault{Kind: domain.FaultOutage, PerHour: 6, DurationSeconds: duration})
	fault := faults.list[0]
	if fault.lookbackSlots != 4 {
		t.Fatalf("the fixture is meant to span 4 steps, got %d", fault.lookbackSlots)
	}

	firstSlot := faultBegin.Unix() / faultStep
	lastSlot := firstSlot + 3000
	began := beganSlots(faults, fault, firstSlot-fault.lookbackSlots, lastSlot)
	covered := 0
	for slot := firstSlot; slot <= lastSlot; slot++ {
		at := time.Unix(slot*faultStep, 0)
		want := false
		//every step that could still be running one: the search must find exactly
		//these and no others
		for back := int64(0); back <= fault.lookbackSlots; back++ {
			source := slot - back
			if began[source] && at.Unix() < source*faultStep+duration {
				want = true
				break
			}
		}
		if want {
			covered++
		}
		_, send := reading(faults, 100, at)
		if send == want {
			t.Fatalf("slot %d: the search says active=%v, the draw says active=%v", slot, !send, want)
		}
	}
	if covered == 0 {
		t.Fatal("the fixture drew no occurrence at all, so the comparison proves nothing")
	}
}

// The search costs one draw per step it looks back over, and that is the whole
// cost cap: it must stop at lookbackSlots steps and not one further. Built by
// hand, because a resolved fault always carries the matching lookback - which is
// exactly what a mutation of the bound would break.
func TestTheLookbackIsNotSearchedBeyondItsLimit(t *testing.T) {
	full := resolvedOf(t, domain.Fault{Kind: domain.FaultOutage, PerHour: 6, DurationSeconds: 4 * faultStep})
	fault := full.list[0]

	firstSlot := faultBegin.Unix() / faultStep
	began := beganSlots(full, fault, firstSlot, firstSlot+3000)
	start := int64(-1)
	for slot := firstSlot; slot <= firstSlot+3000; slot++ {
		//a step that begins one while the step after it does not, so shortening
		//the lookback can be observed one step later
		if began[slot] && !began[slot+1] {
			start = slot
			break
		}
	}
	if start < 0 {
		t.Fatal("the fixture drew no usable occurrence")
	}

	clipped := full
	shortened := fault
	shortened.lookbackSlots = 1
	clipped.list = []resolvedFault{shortened}

	//with the full lookback the occurrence still covers the next step
	if _, send := reading(full, 100, time.Unix((start+1)*faultStep, 0)); send {
		t.Fatal("the occurrence should still cover the step after it began")
	}
	//with a lookback of one step it must not be found there any more
	if _, send := reading(clipped, 100, time.Unix((start+1)*faultStep, 0)); !send {
		t.Fatal("a lookback of one step must not reach back into the previous one")
	}
	//and it is still found in the step it began in
	if _, send := reading(clipped, 100, time.Unix(start*faultStep, 0)); send {
		t.Fatal("a lookback of one step still covers the step the occurrence began in")
	}
}

// The lookback follows from the duration by a ceiling, not a floor: an occurrence
// that runs one second into a further step still has to be found there.
func TestTheLookbackCoversAPartialStep(t *testing.T) {
	faults := resolvedOf(t, domain.Fault{Kind: domain.FaultOutage, PerHour: 6, DurationSeconds: 2*faultStep + 1})
	if got := faults.list[0].lookbackSlots; got != 3 {
		t.Fatalf("an occurrence of 2 steps and a second spans 3 steps, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// the four kinds
// ---------------------------------------------------------------------------

// A freeze repeats the value of the instant its occurrence began. A second
// occurrence is a second defect and takes its own value, which is why the held
// value is keyed by the instant the occurrence started at.
func TestAFreezeHoldsTheValueOfItsFirstInstantAndASecondOccurrenceDoesNot(t *testing.T) {
	first := domain.Fault{Kind: domain.FaultFrozen, From: faultBegin, To: faultBegin.Add(10 * time.Minute)}
	second := domain.Fault{Kind: domain.FaultFrozen, From: faultBegin.Add(time.Hour), To: faultBegin.Add(70 * time.Minute)}
	faults := resolvedOf(t, first, second)
	memory := &faultRun{}
	exchanges := map[string]float64{}

	held, _ := faultedReading(faults, memory, exchanges, 100, faultBegin)
	if held != 100 {
		t.Fatalf("the first instant of a freeze publishes its own value, got %v", held)
	}
	held, _ = faultedReading(faults, memory, exchanges, 175, faultBegin.Add(5*time.Minute))
	if held != 100 {
		t.Fatalf("a freeze repeats the value it holds, got %v", held)
	}
	//outside both windows the raw value goes out again
	held, _ = faultedReading(faults, memory, exchanges, 220, faultBegin.Add(30*time.Minute))
	if held != 220 {
		t.Fatalf("outside the window the reading is undisturbed, got %v", held)
	}
	//the second occurrence must not inherit the first one's held value
	held, _ = faultedReading(faults, memory, exchanges, 300, faultBegin.Add(time.Hour))
	if held != 300 {
		t.Fatalf("a second occurrence holds its own first value, got %v", held)
	}
	held, _ = faultedReading(faults, memory, exchanges, 400, faultBegin.Add(65*time.Minute))
	if held != 300 {
		t.Fatalf("the second occurrence repeats its own value, got %v", held)
	}
}

// The same freeze occurring twice is two defects, and the second one holds its
// own value: the held value is keyed by the instant the occurrence began, not by
// the fault alone. Only a drawn freeze can occur more than once, which is why
// this is the case that pins the key.
func TestASecondOccurrenceOfOneDrawnFreezeHoldsItsOwnValue(t *testing.T) {
	faults := resolvedOf(t, domain.Fault{Kind: domain.FaultFrozen, PerHour: 6, DurationSeconds: 5 * faultStep})
	fault := faults.list[0]
	memory := &faultRun{}
	exchanges := map[string]float64{}

	firstSlot := faultBegin.Unix() / faultStep
	occurrences := 0
	currentBegin := int64(-1)
	held := float64(0)
	for i := int64(0); i < 2000; i++ {
		slot := firstSlot + i
		at := time.Unix(slot*faultStep, 0)
		//strictly increasing, so a value held from an earlier occurrence is
		//visible rather than coincidentally right
		raw := float64(slot)
		began, active := faultAt(fault, faults.seed, faults.channelId, at)
		got, send := faultedReading(faults, memory, exchanges, raw, at)
		if !send {
			t.Fatalf("a freeze never suppresses a send")
		}
		if !active {
			if got != raw {
				t.Fatalf("slot %d: outside an occurrence the reading is undisturbed, got %v", slot, got)
			}
			currentBegin = -1
			continue
		}
		if began != currentBegin {
			occurrences++
			currentBegin, held = began, raw
		}
		if got != held {
			t.Fatalf("slot %d: the occurrence beginning at %d holds %v, got %v", slot, began, held, got)
		}
	}
	if occurrences < 2 {
		t.Fatalf("the fixture needs at least two occurrences to say anything, got %d", occurrences)
	}
}

func TestASpikeScalesTheReadingIncludingByZero(t *testing.T) {
	high := resolvedOf(t, domain.Fault{Kind: domain.FaultSpike, From: faultBegin, To: faultEnd, Factor: 12})
	if got, _ := reading(high, 230, faultBegin); got != 2760 {
		t.Errorf("a factor of 12 on 230 is 2760, got %v", got)
	}
	if got, _ := reading(high, 230, faultEnd); got != 230 {
		t.Errorf("outside the window the reading is undisturbed, got %v", got)
	}
	//a factor of 0 is the sensor that reads nothing: a value, not a silence
	dead := resolvedOf(t, domain.Fault{Kind: domain.FaultSpike, From: faultBegin, To: faultEnd, Factor: 0})
	got, send := reading(dead, 230, faultBegin)
	if !send || got != 0 {
		t.Errorf("a factor of 0 publishes 0, got %v send=%v", got, send)
	}
}

// A meter exchange restarts the register at reset_to and the new one counts on
// from there: the offset is captured once, at the first reading of the exchange,
// and applied to every reading after it.
func TestAMeterExchangeRestartsTheRegisterAndCountsOnFromThere(t *testing.T) {
	faults := resolvedOf(t, domain.Fault{Kind: domain.FaultMeterExchange, From: faultBegin, ResetTo: 0})
	memory := &faultRun{}
	exchanges := map[string]float64{}

	//before the exchange the register reads what the simulation counted
	if got, _ := faultedReading(faults, memory, exchanges, 5000, faultBegin.Add(-time.Minute)); got != 5000 {
		t.Fatalf("before the exchange the reading is undisturbed, got %v", got)
	}
	if len(exchanges) != 0 {
		t.Fatalf("nothing is captured before the exchange, got %v", exchanges)
	}
	if got, _ := faultedReading(faults, memory, exchanges, 5010, faultBegin); got != 0 {
		t.Fatalf("the new register starts at reset_to, got %v", got)
	}
	if len(exchanges) != 1 {
		t.Fatalf("exactly one offset is captured, got %v", exchanges)
	}
	//and it counts on: 40 more in the simulation is 40 on the new register
	if got, _ := faultedReading(faults, memory, exchanges, 5050, faultBegin.Add(time.Hour)); got != 40 {
		t.Fatalf("the new register counts on from reset_to, got %v", got)
	}
}

func TestAMeterExchangeStartsAtANonZeroReading(t *testing.T) {
	faults := resolvedOf(t, domain.Fault{Kind: domain.FaultMeterExchange, From: faultBegin, ResetTo: 120})
	exchanges := map[string]float64{}
	if got, _ := faultedReading(faults, &faultRun{}, exchanges, 5010, faultBegin); got != 120 {
		t.Fatalf("the new register starts at 120, got %v", got)
	}
	if got, _ := faultedReading(faults, &faultRun{}, exchanges, 5030, faultBegin.Add(time.Hour)); got != 140 {
		t.Fatalf("the new register counts on from 120, got %v", got)
	}
}

// Two exchanges compose in document order, each one restarting what the one
// before it produced.
func TestTwoMeterExchangesCompose(t *testing.T) {
	second := faultBegin.Add(2 * time.Hour)
	faults := resolvedOf(t,
		domain.Fault{Kind: domain.FaultMeterExchange, From: faultBegin, ResetTo: 0},
		domain.Fault{Kind: domain.FaultMeterExchange, From: second, ResetTo: 7})
	memory := &faultRun{}
	exchanges := map[string]float64{}

	faultedReading(faults, memory, exchanges, 5000, faultBegin)
	if got, _ := faultedReading(faults, memory, exchanges, 5100, faultBegin.Add(time.Hour)); got != 100 {
		t.Fatalf("after the first exchange the register reads 100, got %v", got)
	}
	if got, _ := faultedReading(faults, memory, exchanges, 5200, second); got != 7 {
		t.Fatalf("the second exchange restarts at 7, got %v", got)
	}
	if got, _ := faultedReading(faults, memory, exchanges, 5250, second.Add(time.Hour)); got != 57 {
		t.Fatalf("the second register counts on from 7, got %v", got)
	}
	if len(exchanges) != 2 {
		t.Fatalf("two exchanges capture two offsets, got %v", exchanges)
	}
}

// An outage suppresses the send and nothing else: a meter exchanged while the
// channel is silent is exchanged when it comes back, so where the outage sits in
// the list does not change the register.
func TestAnOutageSuppressesTheSendWithoutStoppingTheOtherFaults(t *testing.T) {
	faults := resolvedOf(t,
		domain.Fault{Kind: domain.FaultOutage, From: faultBegin, To: faultBegin.Add(time.Hour)},
		domain.Fault{Kind: domain.FaultMeterExchange, From: faultBegin.Add(10 * time.Minute), ResetTo: 0})
	memory := &faultRun{}
	exchanges := map[string]float64{}

	value, send := faultedReading(faults, memory, exchanges, 5000, faultBegin.Add(10*time.Minute))
	if send {
		t.Fatal("the outage has to suppress the send")
	}
	if value != 0 {
		t.Fatalf("the exchange still happens during the outage, got %v", value)
	}
	//after the outage the new register is already counting from the exchange
	value, send = faultedReading(faults, memory, exchanges, 5030, faultBegin.Add(2*time.Hour))
	if !send || value != 30 {
		t.Fatalf("after the outage the new register reads 30, got %v send=%v", value, send)
	}
}

// ---------------------------------------------------------------------------
// the edges of the hull
// ---------------------------------------------------------------------------

// A document without faults is not touched by any of this, which is what keeps
// every stored document byte identical to what it produced before.
func TestAChannelWithoutFaultsIsNotTouched(t *testing.T) {
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), nil, &fakePublisher{})
	env := &environment{id: "env-nofault"}
	binding := channelBinding{channel: faultedChannel()}
	if len(binding.faults.list) != 0 {
		t.Fatal("the fixture is meant to carry no faults")
	}
	value, send := rt.faulted(env, binding, nil, 230.0, time.Now())
	if !send || value != 230.0 {
		t.Fatalf("an undisturbed channel publishes its value, got %v send=%v", value, send)
	}
	if env.state.MeterExchanges != nil || env.dirty {
		t.Fatal("a channel without faults must not touch the state at all")
	}
}

// A string or a boolean has no magnitude to freeze, scale or offset, so only an
// outage can act on it: it either goes out as it is or not at all.
func TestAValueThatIsNotANumberCanOnlyFallSilent(t *testing.T) {
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), nil, &fakePublisher{})
	env := &environment{id: "env-string"}
	channel := faultedChannel(
		domain.Fault{Kind: domain.FaultSpike, From: faultBegin, To: faultEnd, Factor: 12},
		domain.Fault{Kind: domain.FaultOutage, From: faultEnd, To: faultEnd.Add(time.Hour)})
	faults, reasons := newChannelFaults(faultSeed, channel, faultStep)
	if len(reasons) > 0 {
		t.Fatalf("the fixture is meant to resolve, got %v", reasons)
	}
	binding := channelBinding{channel: channel, faults: faults}

	value, send := rt.faulted(env, binding, &faultRun{}, "running", faultBegin)
	if !send || value != "running" {
		t.Fatalf("a spike cannot scale a string, got %v send=%v", value, send)
	}
	_, send = rt.faulted(env, binding, &faultRun{}, "running", faultEnd)
	if send {
		t.Fatal("an outage silences a string too")
	}
}

// The captured offsets are persisted, so the wrapper has to mark the state dirty
// exactly when it captured one - and never otherwise, or every tick of every
// faulted channel would write the state out again.
func TestCapturingAMeterOffsetMarksTheStateDirtyOnce(t *testing.T) {
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), nil, &fakePublisher{})
	env := &environment{id: "env-exchange"}
	channel := faultedChannel(domain.Fault{Kind: domain.FaultMeterExchange, From: faultBegin, ResetTo: 0})
	faults, _ := newChannelFaults(faultSeed, channel, faultStep)
	binding := channelBinding{channel: channel, faults: faults}
	memory := &faultRun{}

	rt.faulted(env, binding, memory, 5000.0, faultBegin.Add(-time.Minute))
	if env.dirty {
		t.Fatal("nothing was captured before the exchange, so nothing is dirty")
	}
	rt.faulted(env, binding, memory, 5010.0, faultBegin)
	if !env.dirty {
		t.Fatal("capturing an offset has to reach the store")
	}
	env.dirty = false
	rt.faulted(env, binding, memory, 5020.0, faultBegin.Add(time.Minute))
	if env.dirty {
		t.Fatal("an offset that was already captured is not a change")
	}
}

// ---------------------------------------------------------------------------
// resolution
// ---------------------------------------------------------------------------

// The exchange key is the instant and the channel, and nothing else: an offset
// belongs to one exchange, and two exchanges are told apart by when they happen.
func TestTheExchangeKeyIsTheInstantAndTheChannel(t *testing.T) {
	if meterExchangeKey("ch", 100) == meterExchangeKey("ch", 200) {
		t.Error("two instants must not share an exchange key")
	}
	if meterExchangeKey("ch-1", 100) == meterExchangeKey("ch-2", 100) {
		t.Error("two channels must not share an exchange key")
	}
	//the channel id comes last, so an id carrying the separator cannot be read as
	//another channel's key
	if meterExchangeKey("a|b", 100) == meterExchangeKey("a", 100) {
		t.Error("an id carrying the separator must not collide")
	}
	if faultDrawKey("ch", 0) == faultDrawKey("ch", 1) {
		t.Error("two faults of one channel must not share a draw key")
	}
	if faultDrawKey("1|fault|x", 0) == faultDrawKey("x", 1) {
		t.Error("the index comes first, so a draw key is unambiguous whatever an id looks like")
	}
}

// Two drawn faults of one channel have to occur independently. They did not: the
// index sat at the end of the draw key and fnv-1a barely carries the last bytes
// into the top 53 bits the draw reads, so the two fired in exactly the same slots
// and a drawn outage swallowed every drawn spike of the same channel. Counted
// rather than sampled, because one coincidence per slot looks like nothing.
func TestTwoDrawnFaultsOfOneChannelOccurIndependently(t *testing.T) {
	const slots = 100000
	const probability = 0.05
	differ := 0
	for slot := int64(0); slot < slots; slot++ {
		first := faultBegan(faultSeed, faultChannel, 0, slot, probability)
		second := faultBegan(faultSeed, faultChannel, 1, slot, probability)
		if first != second {
			differ++
		}
	}
	//two independent draws at p disagree with probability 2p(1-p), here 9.5%.
	//A quarter of that is still four orders of magnitude above the 0 the shared
	//sequence produced, and far enough below to leave the draw its randomness.
	if want := int(2 * probability * (1 - probability) * slots / 4); differ < want {
		t.Errorf("two drawn faults of one channel disagreed in only %d of %d slots, expected at least %d: they are drawing one sequence, not two",
			differ, slots, want)
	}
}

// The same for the two faults sitting on one channel end to end: a drawn outage
// must not swallow every occurrence of a drawn spike beside it.
func TestADrawnOutageDoesNotSwallowEveryDrawnSpikeOfTheSameChannel(t *testing.T) {
	faults := resolvedOf(t,
		domain.Fault{Kind: domain.FaultOutage, PerHour: 6, DurationSeconds: 300},
		domain.Fault{Kind: domain.FaultSpike, PerHour: 6, DurationSeconds: 300, Factor: 12})
	spiked, suppressed, both := 0, 0, 0
	firstSlot := faultBegin.Unix() / faultStep
	for i := int64(0); i < 20000; i++ {
		at := time.Unix((firstSlot+i)*faultStep, 0)
		_, outage := faultAt(faults.list[0], faults.seed, faults.channelId, at)
		_, spike := faultAt(faults.list[1], faults.seed, faults.channelId, at)
		switch {
		case outage && spike:
			both++
		case outage:
			suppressed++
		case spike:
			spiked++
		}
	}
	if spiked == 0 {
		t.Fatalf("every drawn spike fell inside the drawn outage: %d were suppressed, %d overlapped, none went out", suppressed, both)
	}
	if suppressed == 0 {
		t.Fatal("the fixture drew no outage at all, so the comparison proves nothing")
	}
}

// The resolution names what it cannot use and drops only that fault, the way an
// unusable change trigger is named and dropped - a document that bypassed
// validation keeps every defect that does work.
func TestAnUnusableFaultIsNamedAndTheOthersStillApply(t *testing.T) {
	channel := faultedChannel(
		domain.Fault{Kind: "brownout", From: faultBegin, To: faultEnd},
		domain.Fault{Kind: domain.FaultOutage, From: faultBegin, To: faultEnd})
	faults, reasons := newChannelFaults(faultSeed, channel, faultStep)
	if len(reasons) != 1 {
		t.Fatalf("expected exactly one named reason, got %v", reasons)
	}
	if len(faults.list) != 1 || faults.list[0].kind != domain.FaultOutage {
		t.Fatalf("the usable fault has to survive, got %#v", faults.list)
	}
	if _, send := reading(faults, 100, faultBegin); send {
		t.Error("the surviving outage still suppresses")
	}
}

// A meter exchange on a source that does not count up has no register to restart,
// and the runtime has to reach the same answer validation does.
func TestAMeterExchangeOnANonCumulativeSourceIsRefusedByBothSides(t *testing.T) {
	channel := faultedChannel(domain.Fault{Kind: domain.FaultMeterExchange, From: faultBegin})
	channel.Source.Profile.Cumulative = false
	_, reasons := newChannelFaults(faultSeed, channel, faultStep)
	if len(reasons) != 1 {
		t.Fatalf("expected the exchange to be refused, got %v", reasons)
	}
	env := testEnvironment("env-cum", channel)
	if err := domain.Validate(env); err == nil {
		t.Fatal("validation has to refuse the same document")
	}
}

// The step is the evaluation cadence, not the publish interval: resolving against
// the wrong one would place a drawn occurrence on a different grid than a
// reconstruction of the same window uses.
func TestTheDrawnOccurrenceSitsOnTheEvaluationStep(t *testing.T) {
	channel := faultedChannel(domain.Fault{Kind: domain.FaultOutage, PerHour: 6, DurationSeconds: 600})
	channel.IntervalSeconds = 600
	channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 0.1, EvaluateIntervalSeconds: 10}

	onEvaluation, _ := newChannelFaults(faultSeed, channel, 10)
	onPublish, _ := newChannelFaults(faultSeed, channel, 600)
	if onEvaluation.list[0].stepSeconds == onPublish.list[0].stepSeconds {
		t.Fatal("the fixture is meant to distinguish the two steps")
	}

	differ := 0
	for i := 0; i < 500; i++ {
		at := faultBegin.Add(time.Duration(i*10) * time.Second)
		_, one := reading(onEvaluation, 100, at)
		_, two := reading(onPublish, 100, at)
		if one != two {
			differ++
		}
	}
	if differ == 0 {
		t.Fatal("the two steps produced the same occurrences, so the slot source is not observable here")
	}
}
