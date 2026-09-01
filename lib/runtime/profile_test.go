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
	"math"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

// monday 2026-08-24 10:30 UTC
var profileT = time.Date(2026, 8, 24, 10, 30, 0, 0, time.UTC)

func flatProfile(base float64, spread float64) domain.ProfileSource {
	return domain.ProfileSource{Base: base, SpreadPercent: spread}
}

func TestAProfileWithoutSpreadIsTheProductOfItsFactors(t *testing.T) {
	p := domain.ProfileSource{
		Base:           100,
		HourFactors:    []float64{0: 0.1, 10: 1.5, 23: 0.2},
		WeekdayFactors: []float64{0: 2, 6: 0.5},
	}
	//pad to the lengths validation demands
	p.HourFactors = append(p.HourFactors, make([]float64, 24-len(p.HourFactors))...)
	p.WeekdayFactors = append(p.WeekdayFactors, make([]float64, 7-len(p.WeekdayFactors))...)

	//monday 10:30 -> hour factor 1.5, weekday factor 2 (monday is index 0)
	got := profileValue(p, 42, "ch", 30, profileT)
	if got != 100*1.5*2 {
		t.Errorf("expected 300, got %v", got)
	}
}

func TestEmptyFactorListsAreNeutral(t *testing.T) {
	if got := profileValue(flatProfile(75.5, 0), 42, "ch", 30, profileT); got != 75.5 {
		t.Errorf("expected the bare base value, got %v", got)
	}
}

// The reason the spread is a hash and not an RNG stream: the same seed and the
// same clock produce the same value, no matter how often or in which order it
// is asked - which is what makes a run reproducible across restarts.
func TestTheSpreadIsDeterministicForSeedChannelAndSlot(t *testing.T) {
	p := flatProfile(100, 10)
	a := profileValue(p, 42, "ch", 30, profileT)
	b := profileValue(p, 42, "ch", 30, profileT.Add(5*time.Second)) //same 30s slot
	if a != b {
		t.Errorf("same slot has to yield the same value, got %v and %v", a, b)
	}
	if profileValue(p, 43, "ch", 30, profileT) == a {
		t.Error("a different seed has to change the draw")
	}
	if profileValue(p, 42, "other", 30, profileT) == a {
		t.Error("a different channel has to have its own stream")
	}
	if profileValue(p, 42, "ch", 30, profileT.Add(time.Minute)) == a {
		t.Error("a different slot has to redraw")
	}
}

func TestTheSpreadStaysInsideItsBounds(t *testing.T) {
	p := flatProfile(100, 10)
	for slot := int64(0); slot < 1000; slot++ {
		got := profileValue(p, 42, "ch", 30, profileT.Add(time.Duration(slot)*30*time.Second))
		if got < 90 || got > 110 {
			t.Fatalf("10 percent spread on 100 has to stay in [90,110], got %v", got)
		}
	}
}

// The spread has to separate the channels of one site, and the ids of one site
// are the ones that differ least: ch-1 next to ch-2, or two platform service
// urns differing in their last character. fnv-1a carries a byte upwards only
// through the multiplications that follow it, so with the id hashed last those
// pairs drew the same number to within 4e-7 of a span of 2 - a whole hall of
// meters sharing one noise sequence, which is invisible in any single series.
//
// Measured rather than asserted on one instant: over 100k slots, two ids may not
// land within a millionth of each other more often than independent draws would,
// which for a uniform pair on [-1, 1) is about one slot in 1e6.
func TestNeighbouringChannelIdsDrawDifferentSpread(t *testing.T) {
	const slots = 100000
	//one in a thousand: three orders of magnitude of headroom over the ~1e-6
	//two independent draws would give, and four below the 100% the broken order
	//produced
	const tolerated = slots / 1000
	for _, pair := range [][2]string{
		{"ch-1", "ch-2"},
		{"ch-23", "ch-24"},
		{"urn:infai:ses:service:aaa", "urn:infai:ses:service:aab"},
	} {
		near := 0
		for slot := int64(0); slot < slots; slot++ {
			if math.Abs(spreadDraw(4711, pair[0], slot)-spreadDraw(4711, pair[1], slot)) < 1e-6 {
				near++
			}
		}
		if near > tolerated {
			t.Errorf("%v and %v drew within a millionth of each other in %d of %d slots, which is not two draws of their own",
				pair[0], pair[1], near, slots)
		}
	}
}

// The same for the two axes the id must not have broken: one channel's draw has
// to move from slot to slot and from seed to seed.
func TestOneChannelsSpreadMovesWithTheSlotAndTheSeed(t *testing.T) {
	const slots = 100000
	const tolerated = slots / 1000
	nearSlot, nearSeed := 0, 0
	for slot := int64(0); slot < slots; slot++ {
		if math.Abs(spreadDraw(4711, "ch-1", slot)-spreadDraw(4711, "ch-1", slot+1)) < 1e-6 {
			nearSlot++
		}
		if math.Abs(spreadDraw(4711, "ch-1", slot)-spreadDraw(4712, "ch-1", slot)) < 1e-6 {
			nearSeed++
		}
	}
	if nearSlot > tolerated {
		t.Errorf("two neighbouring slots drew within a millionth of each other %d times in %d", nearSlot, slots)
	}
	if nearSeed > tolerated {
		t.Errorf("two neighbouring seeds drew within a millionth of each other %d times in %d", nearSeed, slots)
	}
}

func TestMondayBased(t *testing.T) {
	if mondayBased(time.Monday) != 0 || mondayBased(time.Sunday) != 6 || mondayBased(time.Saturday) != 5 {
		t.Errorf("monday=%d sunday=%d saturday=%d", mondayBased(time.Monday), mondayBased(time.Sunday), mondayBased(time.Saturday))
	}
}

func profileChannel(id string, ref string, interval int64, p domain.ProfileSource) domain.Channel {
	return domain.Channel{
		Id: id, Name: id, Direction: domain.Sensor, ExternalRef: ref, IntervalSeconds: interval,
		Source: domain.Source{Kind: domain.SourceProfile, Profile: &p},
	}
}

func TestAProfileChannelPublishesWithoutAnyScript(t *testing.T) {
	env := testEnvironment("env-prof", profileChannel("ch-1", serviceRefOf("env-prof"), 1, flatProfile(230, 5)))
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(4*time.Second, func() bool { return publisher.count() >= 2 }) {
		t.Fatalf("the profile channel did not publish, count %d", publisher.count())
	}
	for _, value := range publisher.forDevice(deviceRefOf("env-prof")) {
		got, ok := value.(float64)
		if !ok {
			t.Fatalf("expected a bare number, got %T (%v)", value, value)
		}
		if got < 230*0.95 || got > 230*1.05 {
			t.Errorf("5 percent spread on 230 has to stay in [218.5, 241.5], got %v", got)
		}
	}
}

// A cumulative profile is a meter: it only counts up, its state survives via
// the flusher, and the published value is the reading.
func TestACumulativeProfileCountsUpAndPersistsItsReading(t *testing.T) {
	p := flatProfile(3600, 0) //rate 3600 per hour = 1 per second per tick
	p.Cumulative = true
	env := testEnvironment("env-meter", profileChannel("ch-1", serviceRefOf("env-meter"), 1, p))
	publisher := &fakePublisher{}
	states := newFakeStates()
	startRuntime(t, testConfig(50*time.Millisecond), newFakeEnvironments(env), states, publisher)

	if !waitFor(6*time.Second, func() bool { return publisher.count() >= 3 }) {
		t.Fatalf("the meter did not publish, count %d", publisher.count())
	}
	values := publisher.forDevice(deviceRefOf("env-meter"))
	for i := 1; i < len(values); i++ {
		if values[i].(float64) <= values[i-1].(float64) {
			t.Fatalf("a meter has to be strictly increasing, got %v", values)
		}
	}
	//rate 3600/h on a 1s tick is 1 per tick, exactly
	if diff := values[1].(float64) - values[0].(float64); math.Abs(diff-1) > 1e-9 {
		t.Errorf("expected 1 per tick, got %v", diff)
	}
	//the reading reaches the state store, which is what a restart resumes from
	flushed := func() bool {
		for _, saved := range states.savedFor("env-meter") {
			if v, ok := saved.state.Assets[testAssetId]["ch-1"]; ok {
				if f, isNumber := v.(float64); isNumber && f >= 1 {
					return true
				}
			}
		}
		return false
	}
	if !waitFor(4*time.Second, flushed) {
		t.Error("the meter reading was never flushed")
	}
}

func TestACommandOnAProfileChannelAnswersWithoutAdvancingTheMeter(t *testing.T) {
	p := flatProfile(3600, 0)
	p.Cumulative = true
	env := testEnvironment("env-cmd", profileChannel("ch-1", serviceRefOf("env-cmd"), 3600, p))
	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	answers := []interface{}{}
	for i := 0; i < 3; i++ {
		if !rt.HandleCommand(deviceRefOf("env-cmd"), serviceRefOf("env-cmd"), nil, func(resp interface{}) {
			answers = append(answers, resp)
		}) {
			t.Fatal("the command was not accepted")
		}
	}
	if len(answers) != 3 {
		t.Fatalf("expected three answers, got %d", len(answers))
	}
	//three reads, same reading: a command must not advance the meter
	if answers[0] != answers[2] {
		t.Errorf("reading the meter advanced it: %v", answers)
	}
}
