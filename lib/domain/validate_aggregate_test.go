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
	"strings"
	"testing"
)

const aggregateCharacteristic = "urn:infai:ses:characteristic:kwh"

// aggregateEnvironment is the smallest shape the aggregate source is for: a
// meter with an aggregate channel, and one asset below it whose profile channel
// carries the same characteristic.
func aggregateEnvironment(mutate func(*Channel)) Environment {
	channel := Channel{
		Id: "ch-total", Name: "Summe", Direction: Sensor, IntervalSeconds: 60,
		CharacteristicId: aggregateCharacteristic, Unit: "kWh",
		Source: Source{Kind: SourceAggregate},
	}
	if mutate != nil {
		mutate(&channel)
	}
	return Environment{
		Id: "e1", Name: "Werk", Type: IndustrialSite, Owner: "o",
		Zones: []Zone{{Id: "z1", Name: "Halle", Type: ZoneHall,
			Assets: []Asset{
				{Id: "a-total", Name: "Verteilzähler", Kind: AssetMeter,
					ExternalTypeId: "urn:infai:ses:device-type:x",
					Channels:       []Channel{channel}},
				{Id: "a-sub", Name: "Unterzähler", Kind: AssetMeter,
					ExternalTypeId: "urn:infai:ses:device-type:x",
					SubmeteredBy:   "a-total",
					Channels: []Channel{{
						Id: "ch-sub", Name: "Wirkenergie", Direction: Sensor, IntervalSeconds: 60,
						CharacteristicId: aggregateCharacteristic, Unit: "kWh",
						Source: Source{Kind: SourceProfile, Profile: &ProfileSource{Base: 100}},
					}}},
			}}},
	}
}

func TestValidateAcceptsAnAggregateChannel(t *testing.T) {
	if err := Validate(aggregateEnvironment(nil)); err != nil {
		t.Errorf("a meter summing the assets below it has to be storable: %v", err)
	}
}

// An aggregate carrying a variant is the hole the "only one variant may be set"
// rule leaves open: kind aggregate plus exactly one foreign variant passes that
// rule, and would be stored with a configuration nothing ever reads.
func TestValidateRejectsAnAggregateCarryingAVariant(t *testing.T) {
	err := Validate(aggregateEnvironment(func(c *Channel) {
		c.Source.Profile = &ProfileSource{Base: 100}
	}))
	if err == nil || !strings.Contains(err.Error(), "no configuration of its own") {
		t.Errorf("expected the profile next to kind aggregate to be refused, got %v", err)
	}
	assertHasPath(t, err, "zones[0].assets[0].channels[0].source")
}

func expectAggregateProblem(t *testing.T, mutate func(*Channel), fragment string) {
	t.Helper()
	err := Validate(aggregateEnvironment(mutate))
	if err == nil || !strings.Contains(err.Error(), fragment) {
		t.Errorf("expected a problem mentioning %q, got %v", fragment, err)
	}
}

func TestValidateRefusesBrokenAggregates(t *testing.T) {
	//an aggregate sums on the publish tick, like a formula computes on it
	expectAggregateProblem(t, func(c *Channel) { c.Source.IntervalSeconds = 5 }, "no own interval")
	expectAggregateProblem(t, func(c *Channel) { c.Direction = Actuator; c.IntervalSeconds = 0 }, "must be a sensor with an interval")
	expectAggregateProblem(t, func(c *Channel) { c.IntervalSeconds = 0 }, "must be a sensor with an interval")
	//without a characteristic there is nothing to pick the summed channels by
	expectAggregateProblem(t, func(c *Channel) { c.CharacteristicId = "" }, "must name one")
	expectAggregateProblem(t, func(c *Channel) { c.CharacteristicId = "   " }, "must name one")
}

// The characteristic problem is reported at the field, not at the channel: the
// author has to be pointed at the input to fill in.
func TestValidateReportsAMissingCharacteristicAtTheField(t *testing.T) {
	err := Validate(aggregateEnvironment(func(c *Channel) { c.CharacteristicId = "" }))
	assertHasPath(t, err, "zones[0].assets[0].channels[0].characteristic_id")
}

// "not supported" rather than "unknown": the kind exists, it just has no asset
// to sum below when it drives a context key, and "unknown" would send its
// author looking for a typo.
func TestValidateRejectsAnAggregateAsContextSource(t *testing.T) {
	env := aggregateEnvironment(nil)
	env.ContextSources = map[string]Source{"total": {Kind: SourceAggregate, IntervalSeconds: 60}}
	err := Validate(env)
	if err == nil || !strings.Contains(err.Error(), "not supported for context sources") {
		t.Errorf("expected an aggregate context source to be refused as unsupported, got %v", err)
	}
	if strings.Contains(err.Error(), "unknown source kind") {
		t.Errorf("an existing kind must not be reported as unknown: %v", err)
	}
}

// An aggregate without anything sub-metering it is not a validation problem: a
// distribution meter whose sub-meters are added next week reads zero until they
// are, and refusing it would make the order of authoring a rule.
func TestValidateAcceptsAnAggregateWithoutSubmeteredAssets(t *testing.T) {
	env := aggregateEnvironment(nil)
	env.Zones[0].Assets = env.Zones[0].Assets[:1]
	if err := Validate(env); err != nil {
		t.Errorf("an aggregate without children has to be storable: %v", err)
	}
}

// ---------------------------------------------------------------------------
// an aggregate and a measured channel of the same quantity on one asset
// ---------------------------------------------------------------------------

// meterOfTheDistributionAsset is a second channel on the asset that carries the
// aggregate - the shape an author reaches for when the distribution meter has a
// reading of its own.
func meterOfTheDistributionAsset(id string, characteristic string) Channel {
	return Channel{
		Id: id, Name: id, Direction: Sensor, IntervalSeconds: 60,
		CharacteristicId: characteristic, Unit: "kWh",
		Source: Source{Kind: SourceProfile, Profile: &ProfileSource{Base: 100}},
	}
}

func aggregateEnvironmentWith(extra ...Channel) Environment {
	env := aggregateEnvironment(nil)
	env.Zones[0].Assets[0].Channels = append(env.Zones[0].Assets[0].Channels, extra...)
	return env
}

// The double counting the rule exists for: the distribution meter reads 1000
// kWh, which already contains the 40 kWh of the machine below it, and the site
// above sums the meter and the aggregate to 1040.
func TestValidateRejectsAMeasuredChannelOfTheSameQuantityNextToAnAggregate(t *testing.T) {
	err := Validate(aggregateEnvironmentWith(meterOfTheDistributionAsset("ch-main", aggregateCharacteristic)))
	if err == nil || !strings.Contains(err.Error(), "counted twice") {
		t.Errorf("expected a second kWh channel next to the kWh aggregate to be refused, got %v", err)
	}
	//reported at the colliding channel, not at the aggregate: the aggregate is
	//the channel that is meant to stay
	assertHasPath(t, err, "zones[0].assets[0].channels[1]")
}

// The whole point of matching by characteristic: a second channel measuring
// something else is not a second reading of the same quantity, and no aggregate
// above sums it into the same total.
func TestValidateAcceptsAnotherQuantityNextToAnAggregate(t *testing.T) {
	if err := Validate(aggregateEnvironmentWith(
		meterOfTheDistributionAsset("ch-temp", "urn:infai:ses:characteristic:celsius"))); err != nil {
		t.Errorf("a channel of a different characteristic has to stay storable: %v", err)
	}
}

// Two aggregates over the same characteristic on one asset are the same
// collision with both halves generated: identical inputs, two indistinguishable
// totals, and the level above adds them both.
func TestValidateRejectsASecondAggregateOfTheSameQuantity(t *testing.T) {
	second := Channel{
		Id: "ch-total-2", Name: "Summe 2", Direction: Sensor, IntervalSeconds: 60,
		CharacteristicId: aggregateCharacteristic, Unit: "kWh",
		Source: Source{Kind: SourceAggregate},
	}
	err := Validate(aggregateEnvironmentWith(second))
	if err == nil || !strings.Contains(err.Error(), "counted twice") {
		t.Errorf("expected the second aggregate over the same characteristic to be refused, got %v", err)
	}
	assertHasPath(t, err, "zones[0].assets[0].channels[1]")
}

// A characteristic differing only in whitespace is the same characteristic to
// the runtime, which trims both sides before matching, so it has to collide
// here as well - otherwise a trailing space would be the way around the rule.
func TestValidateRejectsTheSameQuantityWrittenWithWhitespace(t *testing.T) {
	err := Validate(aggregateEnvironmentWith(meterOfTheDistributionAsset("ch-main", aggregateCharacteristic+" ")))
	if err == nil || !strings.Contains(err.Error(), "counted twice") {
		t.Errorf("expected a trailing space not to buy a second channel of the same quantity, got %v", err)
	}
}

// An actuator is not a second reading: it publishes nothing on a schedule and
// no aggregate sums it, so a setpoint carrying the same characteristic stays
// storable next to the total.
func TestValidateAcceptsAnActuatorOfTheSameQuantityNextToAnAggregate(t *testing.T) {
	setpoint := Channel{
		Id: "ch-setpoint", Name: "Sollwert", Direction: Actuator,
		CharacteristicId: aggregateCharacteristic, Unit: "kWh",
		Source: Source{Kind: SourceScript, Script: &ScriptSource{Code: "1;"}},
	}
	if err := Validate(aggregateEnvironmentWith(setpoint)); err != nil {
		t.Errorf("an actuator of the same characteristic has to stay storable: %v", err)
	}
}

// The remainder pattern the rule points at: the distribution meter's own share
// becomes an asset of its own below it, so every level is summed exactly once.
// It needs no device of its own to be a legal document.
func TestValidateAcceptsTheOwnShareAsASubmeteredAssetOfItsOwn(t *testing.T) {
	env := aggregateEnvironment(nil)
	env.Zones[0].Assets = append(env.Zones[0].Assets, Asset{
		Id: "a-own-share", Name: "Eigenanteil", Kind: AssetMeter,
		ExternalTypeId: "urn:infai:ses:device-type:x",
		SubmeteredBy:   "a-total",
		Channels:       []Channel{meterOfTheDistributionAsset("ch-own-share", aggregateCharacteristic)},
	})
	if err := Validate(env); err != nil {
		t.Errorf("the own share as a sub-metered asset has to be storable: %v", err)
	}
}
