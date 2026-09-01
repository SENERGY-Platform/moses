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

package domain

import (
	"math"
	"strings"
	"testing"
	"time"
)

// the one channel of validEnvironment, which every case below edits
const faultChannelPath = "zones[0].assets[0].channels[0]"

var (
	faultFrom = time.Date(2026, 7, 1, 6, 0, 0, 0, time.UTC)
	faultTo   = time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
)

// faultEnvironment is validEnvironment with a cumulative profile on its channel,
// which is the one source shape every fault kind including the meter exchange is
// allowed on. The channel publishes every 60 seconds and has no change trigger,
// so its evaluation step is that interval.
func faultEnvironment(faults ...Fault) Environment {
	env := validEnvironment()
	channel := &env.Zones[0].Assets[0].Channels[0]
	channel.IntervalSeconds = 60
	channel.Source = Source{Kind: SourceProfile, Profile: &ProfileSource{Base: 10, Cumulative: true}}
	channel.Faults = faults
	return env
}

func outageWindow() Fault {
	return Fault{Kind: FaultOutage, From: faultFrom, To: faultTo}
}

// A document that carries no faults at all is untouched by any of this, which is
// the additivity the whole block rests on.
func TestValidateAcceptsAChannelWithoutFaults(t *testing.T) {
	if err := Validate(faultEnvironment()); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAcceptsTheFourFaultKinds(t *testing.T) {
	env := faultEnvironment(
		outageWindow(),
		Fault{Kind: FaultFrozen, PerHour: 2, DurationSeconds: 300},
		Fault{Kind: FaultSpike, From: faultFrom, To: faultTo, Factor: 12},
		Fault{Kind: FaultMeterExchange, From: faultTo, ResetTo: 0},
	)
	if err := Validate(env); err != nil {
		t.Fatal(err)
	}
}

// Overlapping windows are allowed on purpose: they compose in document order,
// which is how "the meter froze and was then exchanged" is written down.
func TestValidateAcceptsOverlappingFaultWindows(t *testing.T) {
	env := faultEnvironment(
		Fault{Kind: FaultFrozen, From: faultFrom, To: faultTo},
		Fault{Kind: FaultSpike, From: faultFrom.Add(time.Hour), To: faultTo.Add(time.Hour), Factor: 3},
	)
	if err := Validate(env); err != nil {
		t.Fatal(err)
	}
}

// A spike that reads zero is a real, named defect - the sensor that returns
// nothing - so 0 has to be storable while 1 is not.
func TestValidateAcceptsASpikeFactorOfZero(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultSpike, From: faultFrom, To: faultTo, Factor: 0})
	if err := Validate(env); err != nil {
		t.Fatal(err)
	}
}

// rule 1: the list is bounded, and the refusal does not walk it
func TestValidateRejectsMoreFaultsThanTheLimit(t *testing.T) {
	faults := make([]Fault, MaxChannelFaults+1)
	for i := range faults {
		faults[i] = outageWindow()
	}
	err := Validate(faultEnvironment(faults...))
	assertHasPath(t, err, faultChannelPath+".faults")
	if paths := problemPaths(t, err); len(paths) != 1 {
		t.Fatalf("the refusal must not walk the list, got %v", paths)
	}
}

// rule 1, the other side: exactly the limit is storable
func TestValidateAcceptsExactlyTheFaultLimit(t *testing.T) {
	faults := make([]Fault, MaxChannelFaults)
	for i := range faults {
		faults[i] = outageWindow()
	}
	if err := Validate(faultEnvironment(faults...)); err != nil {
		t.Fatal(err)
	}
}

// rule 2
func TestValidateRejectsAnUnknownFaultKind(t *testing.T) {
	env := faultEnvironment(Fault{Kind: "brownout", From: faultFrom, To: faultTo})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].kind")
}

// rule 3: an actuator publishes no reading of its own, so there is nothing to
// disturb
func TestValidateRejectsAFaultOnAnActuator(t *testing.T) {
	env := faultEnvironment(outageWindow())
	channel := &env.Zones[0].Assets[0].Channels[0]
	channel.Direction = Actuator
	channel.IntervalSeconds = 0
	channel.Source = Source{Kind: SourceScript, Script: &ScriptSource{Code: "moses.service.send(1);"}}
	assertHasPath(t, Validate(env), faultChannelPath+".faults")
}

// rule 3, the other half
func TestValidateRejectsAFaultOnAChannelWithoutAnInterval(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultFrozen, PerHour: 1, DurationSeconds: 60})
	channel := &env.Zones[0].Assets[0].Channels[0]
	channel.IntervalSeconds = 0
	assertHasPath(t, Validate(env), faultChannelPath+".faults")
}

// rule 4
func TestValidateRejectsAFaultWithBothAWindowAndARate(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultOutage, From: faultFrom, To: faultTo, PerHour: 1, DurationSeconds: 60})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0]")
}

func TestValidateRejectsAFaultWithNeitherAWindowNorARate(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultOutage})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0]")
}

// rule 5: the same instant rule a dated change follows, and for the same reason
func TestValidateRejectsAFaultInstantWithAFraction(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultOutage, From: faultFrom.Add(500 * time.Millisecond), To: faultTo})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].from")
}

func TestValidateRejectsAFaultInstantOutsideTheExpressibleRange(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultOutage,
		From: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), To: faultTo})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].from")
}

// rule 6: the end is exclusive, so an empty window is a fault that never occurs
func TestValidateRejectsAFaultWindowThatDoesNotEndAfterItBegins(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultOutage, From: faultFrom, To: faultFrom})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].to")
}

func TestValidateRejectsAFaultWindowWithoutAStart(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultOutage, To: faultTo})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].from")
}

// rule 7
func TestValidateRejectsAMeterExchangeWithAnEnd(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultMeterExchange, From: faultFrom, To: faultTo})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].to")
}

func TestValidateRejectsAMeterExchangeDrawnAtARate(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultMeterExchange, PerHour: 1, DurationSeconds: 60})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].per_hour")
}

// rule 8
func TestValidateRejectsARateThatIsNotPositive(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultOutage, PerHour: -1, DurationSeconds: 60})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].per_hour")
}

func TestValidateRejectsARateThatIsNotFinite(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultOutage, PerHour: math.NaN(), DurationSeconds: 60})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].per_hour")
}

// rule 9
func TestValidateRejectsADrawnOccurrenceOfNoLength(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultOutage, PerHour: 1, DurationSeconds: 0, Factor: 0, ResetTo: 0, From: time.Time{}, To: time.Time{}})
	//per_hour alone already makes it a rate, so duration_seconds of 0 is the
	//field under test rather than "neither window nor rate"
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].duration_seconds")
}

// rule 10: the message has to name both numbers and the way out, or the author
// cannot tell how much too long the occurrence is
func TestValidateRejectsADrawnOccurrenceBeyondTheLookback(t *testing.T) {
	//60 second step, so 64 steps is 3840 seconds and one more second is 65
	env := faultEnvironment(Fault{Kind: FaultOutage, PerHour: 0.1, DurationSeconds: 60*MaxFaultLookbackSlots + 1})
	err := Validate(env)
	assertHasPath(t, err, faultChannelPath+".faults[0].duration_seconds")
	message := err.Error()
	for _, want := range []string{"65", "64", "60", "window"} {
		if !strings.Contains(message, want) {
			t.Errorf("the message has to name %q, got %v", want, message)
		}
	}
}

// rule 10, the other side: exactly the lookback is storable
func TestValidateAcceptsADrawnOccurrenceExactlyAtTheLookback(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultOutage, PerHour: 0.1, DurationSeconds: 60 * MaxFaultLookbackSlots})
	if err := Validate(env); err != nil {
		t.Fatal(err)
	}
}

// rule 11: one draw per step cannot express two occurrences beginning in it
func TestValidateRejectsARateFasterThanOneOccurrencePerStep(t *testing.T) {
	//60 second step: 60 per hour is exactly one per step and still allowed
	env := faultEnvironment(Fault{Kind: FaultOutage, PerHour: 61, DurationSeconds: 30})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].per_hour")
}

func TestValidateAcceptsARateOfExactlyOneOccurrencePerStep(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultOutage, PerHour: 60, DurationSeconds: 30})
	if err := Validate(env); err != nil {
		t.Fatal(err)
	}
}

// rule 12
func TestValidateRejectsAFactorOnAKindThatIgnoresIt(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultOutage, From: faultFrom, To: faultTo, Factor: 3})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].factor")
}

func TestValidateRejectsASpikeFactorOfOne(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultSpike, From: faultFrom, To: faultTo, Factor: 1})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].factor")
}

func TestValidateRejectsASpikeFactorThatIsNotFinite(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultSpike, From: faultFrom, To: faultTo, Factor: math.Inf(1)})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].factor")
}

// rule 13
func TestValidateRejectsAResetToOnAKindThatIgnoresIt(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultOutage, From: faultFrom, To: faultTo, ResetTo: 5})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].reset_to")
}

func TestValidateRejectsANegativeResetTo(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultMeterExchange, From: faultFrom, ResetTo: -1})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].reset_to")
}

func TestValidateRejectsAResetToThatIsNotFinite(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultMeterExchange, From: faultFrom, ResetTo: math.NaN()})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].reset_to")
}

// rule 15: the offset a meter exchange stores is identified by its instant, so
// two of them at one instant would restart one register from one stored offset
func TestValidateRejectsTwoMeterExchangesAtTheSameInstant(t *testing.T) {
	env := faultEnvironment(
		Fault{Kind: FaultMeterExchange, From: faultFrom, ResetTo: 0},
		Fault{Kind: FaultMeterExchange, From: faultFrom, ResetTo: 100})
	assertHasPath(t, Validate(env), faultChannelPath+".faults[1].from")
}

func TestValidateAcceptsTwoMeterExchangesAtDifferentInstants(t *testing.T) {
	env := faultEnvironment(
		Fault{Kind: FaultMeterExchange, From: faultFrom, ResetTo: 0},
		Fault{Kind: FaultMeterExchange, From: faultTo, ResetTo: 100})
	if err := Validate(env); err != nil {
		t.Fatal(err)
	}
}

// rule 14: a register that does not count up has nothing to restart
func TestValidateRejectsAMeterExchangeOnANonCumulativeSource(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultMeterExchange, From: faultFrom})
	env.Zones[0].Assets[0].Channels[0].Source.Profile.Cumulative = false
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].kind")
}

func TestValidateAcceptsAMeterExchangeOnACumulativeDataset(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultMeterExchange, From: faultFrom, ResetTo: 1000})
	env.Zones[0].Assets[0].Channels[0].Source = Source{Kind: SourceDataset, Dataset: &DatasetSource{
		Origin: OriginFile, Ref: "d1", Resample: ResampleHold, Anchor: AnchorLoop, Cumulative: true,
	}}
	if err := Validate(env); err != nil {
		t.Fatal(err)
	}
}

// A trigger moves the evaluation onto its own cadence, and both step dependent
// rules have to be measured against that cadence rather than against the
// heartbeat - otherwise a ten second evaluation would be judged as a ten minute
// one and a far too fast rate would pass.
func TestValidateMeasuresAFaultAgainstTheEvaluationCadence(t *testing.T) {
	env := faultEnvironment(Fault{Kind: FaultOutage, PerHour: 400, DurationSeconds: 30})
	channel := &env.Zones[0].Assets[0].Channels[0]
	channel.IntervalSeconds = 600
	channel.PublishOnChange = &ChangeTrigger{Absolute: 0.1, EvaluateIntervalSeconds: 10}
	//400 per hour over a 10 second step is 1.11 occurrences per step
	assertHasPath(t, Validate(env), faultChannelPath+".faults[0].per_hour")

	channel.PublishOnChange.EvaluateIntervalSeconds = 5
	//over a 5 second step it is 0.55, and the same document is fine
	if err := Validate(env); err != nil {
		t.Fatal(err)
	}
}

func TestChannelStepSecondsFollowsTheEvaluationCadence(t *testing.T) {
	plain := Channel{IntervalSeconds: 600}
	if got := channelStepSeconds(plain); got != 600 {
		t.Errorf("without a trigger the step is the publish interval, got %d", got)
	}
	triggered := Channel{IntervalSeconds: 600, PublishOnChange: &ChangeTrigger{EvaluateIntervalSeconds: 10}}
	if got := channelStepSeconds(triggered); got != 10 {
		t.Errorf("with an evaluation interval the step is that interval, got %d", got)
	}
	sourced := Channel{IntervalSeconds: 600, PublishOnChange: &ChangeTrigger{},
		Source: Source{IntervalSeconds: 5}}
	if got := channelStepSeconds(sourced); got != 5 {
		t.Errorf("with a source interval the step is that interval, got %d", got)
	}
}
