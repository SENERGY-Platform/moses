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
)

// the one channel of validEnvironment, which every case below edits
const triggerChannelPath = "zones[0].assets[0].channels[0]"

// changeTriggerEnvironment is validEnvironment with a usable trigger on its
// channel: a ten minute heartbeat and a value computed every ten seconds, which
// is the shape a real meter has.
func changeTriggerEnvironment(mutate func(channel *Channel)) Environment {
	env := validEnvironment()
	channel := &env.Zones[0].Assets[0].Channels[0]
	channel.IntervalSeconds = 600
	channel.PublishOnChange = &ChangeTrigger{Absolute: 0.1, EvaluateIntervalSeconds: 10}
	if mutate != nil {
		mutate(channel)
	}
	return env
}

func TestValidateAcceptsAChangeTriggerWithItsOwnEvaluationInterval(t *testing.T) {
	if err := Validate(changeTriggerEnvironment(nil)); err != nil {
		t.Fatal(err)
	}
}

// The second legitimate form: the source already computes on an interval of its
// own, so that interval is when a change can be noticed and the trigger must not
// name a second one.
func TestValidateAcceptsAChangeTriggerOnASourceThatCarriesItsOwnInterval(t *testing.T) {
	env := changeTriggerEnvironment(func(channel *Channel) {
		channel.Source.IntervalSeconds = 5
		channel.PublishOnChange = &ChangeTrigger{Relative: 0.05}
	})
	if err := Validate(env); err != nil {
		t.Fatal(err)
	}
}

// An actuator is driven from outside and publishes no reading of its own, so
// there is nothing whose change could be published.
func TestValidateRejectsAChangeTriggerOnAnActuator(t *testing.T) {
	env := changeTriggerEnvironment(func(channel *Channel) {
		channel.Direction = Actuator
	})
	assertHasPath(t, Validate(env), triggerChannelPath+".publish_on_change")
}

// Without a heartbeat a value that stops moving is never sent again, and the
// channel goes silent in a way that looks like a dead device.
func TestValidateRejectsAChangeTriggerWithoutAHeartbeatInterval(t *testing.T) {
	env := changeTriggerEnvironment(func(channel *Channel) {
		channel.IntervalSeconds = 0
	})
	assertHasPath(t, Validate(env), triggerChannelPath+".interval_seconds")
}

func TestValidateRejectsANegativeAbsoluteThreshold(t *testing.T) {
	env := changeTriggerEnvironment(func(channel *Channel) {
		channel.PublishOnChange.Absolute = -1
	})
	assertHasPath(t, Validate(env), triggerChannelPath+".publish_on_change.absolute")
}

func TestValidateRejectsANegativeRelativeThreshold(t *testing.T) {
	env := changeTriggerEnvironment(func(channel *Channel) {
		channel.PublishOnChange.Relative = -0.5
	})
	assertHasPath(t, Validate(env), triggerChannelPath+".publish_on_change.relative")
}

// NaN is never greater than anything and infinity is never exceeded, so either
// of them disables the threshold while looking like a configured one. Both are
// what a generated document produces from a division.
func TestValidateRejectsAThresholdThatIsNotAFiniteNumber(t *testing.T) {
	env := changeTriggerEnvironment(func(channel *Channel) {
		channel.PublishOnChange.Absolute = math.NaN()
		channel.PublishOnChange.Relative = math.Inf(1)
	})
	err := Validate(env)
	assertHasPath(t, err, triggerChannelPath+".publish_on_change.absolute")
	assertHasPath(t, err, triggerChannelPath+".publish_on_change.relative")
}

func TestValidateRejectsANegativeEvaluationInterval(t *testing.T) {
	env := changeTriggerEnvironment(func(channel *Channel) {
		channel.PublishOnChange.EvaluateIntervalSeconds = -10
	})
	assertHasPath(t, Validate(env), triggerChannelPath+".publish_on_change.evaluate_interval_seconds")
}

// A trigger without a threshold is a plain ticker wearing the name of an event
// driven one.
func TestValidateRejectsAChangeTriggerWithoutAnyThreshold(t *testing.T) {
	env := changeTriggerEnvironment(func(channel *Channel) {
		channel.PublishOnChange.Absolute = 0
		channel.PublishOnChange.Relative = 0
	})
	err := Validate(env)
	assertHasPath(t, err, triggerChannelPath+".publish_on_change")
	if !strings.Contains(err.Error(), "greater than zero") {
		t.Fatalf("expected the message to say what is missing, got %v", err)
	}
}

// One channel has exactly one evaluation cadence. Both fields set is a
// contradiction with no reading that could be called correct.
func TestValidateRejectsTwoEvaluationCadences(t *testing.T) {
	env := changeTriggerEnvironment(func(channel *Channel) {
		channel.Source.IntervalSeconds = 5
	})
	assertHasPath(t, Validate(env), triggerChannelPath+".publish_on_change.evaluate_interval_seconds")
}

func TestValidateRejectsAChangeTriggerWithNoEvaluationCadenceAtAll(t *testing.T) {
	env := changeTriggerEnvironment(func(channel *Channel) {
		channel.Source.IntervalSeconds = 0
		channel.PublishOnChange.EvaluateIntervalSeconds = 0
	})
	assertHasPath(t, Validate(env), triggerChannelPath+".publish_on_change.evaluate_interval_seconds")
}

// Evaluating more rarely than the heartbeat fires means the heartbeat is always
// first, so the trigger could never be the reason for a publish. Both forms of
// the cadence are reported at the field that carries it.
func TestValidateRejectsAnEvaluationSlowerThanTheHeartbeat(t *testing.T) {
	own := changeTriggerEnvironment(func(channel *Channel) {
		channel.IntervalSeconds = 60
		channel.PublishOnChange.EvaluateIntervalSeconds = 61
	})
	assertHasPath(t, Validate(own), triggerChannelPath+".publish_on_change.evaluate_interval_seconds")

	fromSource := changeTriggerEnvironment(func(channel *Channel) {
		channel.IntervalSeconds = 60
		channel.Source.IntervalSeconds = 61
		channel.PublishOnChange = &ChangeTrigger{Absolute: 1}
	})
	assertHasPath(t, Validate(fromSource), triggerChannelPath+".source.interval_seconds")

	// and the boundary is inclusive: evaluating exactly as often as the
	// heartbeat fires is the densest legal shape, not one second too slow
	equal := changeTriggerEnvironment(func(channel *Channel) {
		channel.IntervalSeconds = 60
		channel.PublishOnChange.EvaluateIntervalSeconds = 60
	})
	if err := Validate(equal); err != nil {
		t.Fatalf("an evaluation exactly as often as the heartbeat has to be accepted, got %v", err)
	}
}

// A document without the field must not be touched by any of the rules above:
// this is the shape every stored document has.
func TestValidateIgnoresAChannelWithoutAChangeTrigger(t *testing.T) {
	env := validEnvironment()
	env.Zones[0].Assets[0].Channels[0].PublishOnChange = nil
	if err := Validate(env); err != nil {
		t.Fatal(err)
	}
}
