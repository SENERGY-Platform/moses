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
	"bytes"
	"context"
	"log/slog"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/util"
)

// The two characteristics of these tests. Matching by characteristic is what
// keeps an aggregate from summing kilowatt hours and degrees into one number.
const (
	energyCharacteristic = "urn:infai:ses:characteristic:kwh"
	otherCharacteristic  = "urn:infai:ses:characteristic:celsius"
)

// ---------------------------------------------------------------------------
// builders: testEnvironment has exactly one asset, a meter tree needs several
// ---------------------------------------------------------------------------

// treeAsset is one asset of a meter tree: its own id, the asset that meters it
// too, and its channels.
type treeAsset struct {
	id           string
	submeteredBy string
	channels     []domain.Channel
}

// treeEnvironment hangs every asset into one zone, each with a platform device
// of its own, so two assets of the same test never share a device by accident.
// The tree is expressed by submetered_by, not by nesting: the model has no
// asset-in-asset.
func treeEnvironment(envId string, assets ...treeAsset) domain.Environment {
	result := domain.Environment{
		Id:      envId,
		Name:    envId,
		Type:    domain.IndustrialSite,
		Owner:   "test-owner",
		Context: map[string]interface{}{},
		Zones: []domain.Zone{{
			Id:            testZoneId,
			Name:          "hall",
			Type:          domain.ZoneHall,
			InitialStates: map[string]interface{}{},
		}},
	}
	for _, asset := range assets {
		result.Zones[0].Assets = append(result.Zones[0].Assets, domain.Asset{
			Id:             asset.id,
			Name:           asset.id,
			Kind:           domain.AssetMeter,
			ExternalRef:    deviceRefOf(envId) + "-" + asset.id,
			ExternalTypeId: "urn:infai:ses:device-type:test",
			SubmeteredBy:   asset.submeteredBy,
			InitialStates:  map[string]interface{}{},
			Channels:       asset.channels,
		})
	}
	return result
}

// aggregateChannel is a channel summing what the assets below it publish. It
// carries no source configuration at all - the tree is the configuration.
func aggregateChannel(id string, ref string, interval int64, characteristic string) domain.Channel {
	return domain.Channel{
		Id: id, Name: id, Direction: domain.Sensor, ExternalRef: ref,
		CharacteristicId: characteristic, IntervalSeconds: interval,
		Source: domain.Source{Kind: domain.SourceAggregate},
	}
}

// measuringChannel is a constant profile channel: base value, no spread, so the
// sum a test asserts on is an exact number.
func measuringChannel(id string, ref string, characteristic string, base float64) domain.Channel {
	channel := profileChannel(id, ref, 1, flatProfile(base, 0))
	channel.CharacteristicId = characteristic
	return channel
}

func serviceOf(envId string, channelId string) string {
	return serviceRefOf(envId) + "-" + channelId
}

// valuesOn returns everything one platform service received, in order.
func valuesOn(publisher *fakePublisher, serviceRef string) []interface{} {
	result := []interface{}{}
	for _, event := range publisher.all() {
		if event.serviceRef == serviceRef {
			result = append(result, event.value)
		}
	}
	return result
}

func sawValue(publisher *fakePublisher, serviceRef string, want float64) bool {
	for _, value := range valuesOn(publisher, serviceRef) {
		if value == want {
			return true
		}
	}
	return false
}

// assertOnlyValues fails on any published value outside the allowed set. It is
// the half of an aggregate assertion that catches summing too much: the wanted
// total shows up eventually either way, a wrong intermediate total only ever
// appears here.
func assertOnlyValues(t *testing.T, publisher *fakePublisher, serviceRef string, allowed ...float64) {
	t.Helper()
	for _, value := range valuesOn(publisher, serviceRef) {
		ok := false
		for _, candidate := range allowed {
			if value == candidate {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("%v published %v, allowed are %v", serviceRef, value, allowed)
		}
	}
}

// ---------------------------------------------------------------------------
// the running sum
// ---------------------------------------------------------------------------

// The case the aggregate source exists for: a distribution meter reads what the
// meters below it read, and nobody has to keep a formula in step with the tree.
func TestAnAggregateSumsTheSubmeteredChannels(t *testing.T) {
	const envId = "env-agg-sum"
	env := treeEnvironment(envId,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", serviceOf(envId, "total"), 1, energyCharacteristic)}},
		treeAsset{id: "a-sub-1", submeteredBy: "a-total", channels: []domain.Channel{
			measuringChannel("ch-sub-1", serviceOf(envId, "sub-1"), energyCharacteristic, 10)}},
		treeAsset{id: "a-sub-2", submeteredBy: "a-total", channels: []domain.Channel{
			measuringChannel("ch-sub-2", serviceOf(envId, "sub-2"), energyCharacteristic, 4)}},
	)
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(8*time.Second, func() bool { return sawValue(publisher, serviceOf(envId, "total"), 14.0) }) {
		t.Fatalf("the aggregate never published 10+4=14, it published %v", valuesOn(publisher, serviceOf(envId, "total")))
	}
	//0 before the sub-meters have ticked once, 10 or 4 in between: what must
	//never appear is a total above the sum
	assertOnlyValues(t, publisher, serviceOf(envId, "total"), 0, 4, 10, 14)
}

// The characteristic is what picks the channels to sum. Without it an aggregate
// over a meter that also reports a temperature would add the two.
func TestAnAggregateMatchesByCharacteristic(t *testing.T) {
	const envId = "env-agg-char"
	env := treeEnvironment(envId,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", serviceOf(envId, "total"), 1, energyCharacteristic)}},
		treeAsset{id: "a-sub", submeteredBy: "a-total", channels: []domain.Channel{
			measuringChannel("ch-energy", serviceOf(envId, "energy"), energyCharacteristic, 10),
			measuringChannel("ch-temp", serviceOf(envId, "temp"), otherCharacteristic, 100),
		}},
	)
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(8*time.Second, func() bool { return sawValue(publisher, serviceOf(envId, "total"), 10.0) }) {
		t.Fatalf("the aggregate never published the energy channel alone, it published %v",
			valuesOn(publisher, serviceOf(envId, "total")))
	}
	//110 would be the temperature summed into the energy total
	assertOnlyValues(t, publisher, serviceOf(envId, "total"), 0, 10)
}

// An asset that hangs on the zone is not part of anybody's meter tree, however
// much it looks like the one next to it.
func TestAnAggregateIgnoresAssetsThatHangOnTheZone(t *testing.T) {
	const envId = "env-agg-zone"
	env := treeEnvironment(envId,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", serviceOf(envId, "total"), 1, energyCharacteristic)}},
		treeAsset{id: "a-sub", submeteredBy: "a-total", channels: []domain.Channel{
			measuringChannel("ch-sub", serviceOf(envId, "sub"), energyCharacteristic, 10)}},
		treeAsset{id: "a-loose", channels: []domain.Channel{
			measuringChannel("ch-loose", serviceOf(envId, "loose"), energyCharacteristic, 4)}},
	)
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(8*time.Second, func() bool { return sawValue(publisher, serviceOf(envId, "total"), 10.0) }) {
		t.Fatalf("the aggregate never published the sub-metered channel alone, it published %v",
			valuesOn(publisher, serviceOf(envId, "total")))
	}
	assertOnlyValues(t, publisher, serviceOf(envId, "total"), 0, 10)
}

// A tree deeper than one level: the top meter sums the totals of the level
// below, not the leaves - which is why an intermediate aggregate is summed like
// any other channel. Each level costs one publish tick, so this is a wait, not
// an instant.
//
// The middle asset carries its aggregate and nothing else. A measured channel of
// the same characteristic next to it is refused by validation
// (lib/domain/validate.go, checkAggregateOverlap), because the level above sums
// both and counts the whole sub-tree twice; the middle meter's own share is
// therefore an asset of its own, sub-metered by it. That is the modelling
// pattern for a distribution meter's own consumption, not a detour taken for
// this test.
func TestANestedAggregateFollowsTheTree(t *testing.T) {
	const envId = "env-agg-nested"
	env := treeEnvironment(envId,
		treeAsset{id: "a-top", channels: []domain.Channel{
			aggregateChannel("ch-top", serviceOf(envId, "top"), 1, energyCharacteristic)}},
		treeAsset{id: "a-mid", submeteredBy: "a-top", channels: []domain.Channel{
			aggregateChannel("ch-mid", serviceOf(envId, "mid"), 1, energyCharacteristic)}},
		treeAsset{id: "a-mid-own", submeteredBy: "a-mid", channels: []domain.Channel{
			measuringChannel("ch-mid-own", serviceOf(envId, "mid-own"), energyCharacteristic, 4)}},
		treeAsset{id: "a-leaf", submeteredBy: "a-mid", channels: []domain.Channel{
			measuringChannel("ch-leaf", serviceOf(envId, "leaf"), energyCharacteristic, 10)}},
	)
	if err := domain.Validate(env); err != nil {
		t.Fatalf("the tree has to be a storable document: %v", err)
	}
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	//the middle level sums the leaf and the own share below it: 10 + 4
	if !waitFor(8*time.Second, func() bool { return sawValue(publisher, serviceOf(envId, "mid"), 14.0) }) {
		t.Fatalf("the middle aggregate never published 14, it published %v",
			valuesOn(publisher, serviceOf(envId, "mid")))
	}
	//and the top sums that one total, reaching exactly one level down
	if !waitFor(8*time.Second, func() bool { return sawValue(publisher, serviceOf(envId, "top"), 14.0) }) {
		t.Fatalf("the top aggregate never published 14, it published %v", valuesOn(publisher, serviceOf(envId, "top")))
	}
	//28 would be the sub-tree counted twice: once through the middle aggregate
	//and once by the top reaching past its own children
	assertOnlyValues(t, publisher, serviceOf(envId, "mid"), 0, 4, 10, 14)
	assertOnlyValues(t, publisher, serviceOf(envId, "top"), 0, 4, 10, 14)
}

// A channel that publishes nowhere still counts. Its value is remembered before
// the publish is even attempted (dispatch, publish), which is what lets an asset
// stand for the unmeasured remainder of a meter without sending a reading of its
// own to the platform.
func TestAnAggregateCountsAChildThatCannotPublish(t *testing.T) {
	const envId = "env-agg-mute"
	mute := measuringChannel("ch-mute", "", energyCharacteristic, 10)
	env := treeEnvironment(envId,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", serviceOf(envId, "total"), 1, energyCharacteristic)}},
		treeAsset{id: "a-sub", submeteredBy: "a-total", channels: []domain.Channel{mute}},
	)
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(8*time.Second, func() bool { return sawValue(publisher, serviceOf(envId, "total"), 10.0) }) {
		t.Fatalf("the aggregate did not count the channel without a platform service, it published %v",
			valuesOn(publisher, serviceOf(envId, "total")))
	}
	if published := len(valuesOn(publisher, "")); published != 0 {
		t.Errorf("the channel without a service ref published %d readings", published)
	}
}

// Zero is a reading. A distribution meter that has no sub-meters yet reads
// nothing flowing through it, and a channel that stays silent instead looks
// broken to everything downstream.
func TestAnAggregateWithoutChildrenPublishesZero(t *testing.T) {
	const envId = "env-agg-empty"
	env := treeEnvironment(envId,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", serviceOf(envId, "total"), 1, energyCharacteristic)}},
	)
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(6*time.Second, func() bool { return sawValue(publisher, serviceOf(envId, "total"), 0.0) }) {
		t.Fatalf("an aggregate without children has to publish 0, it published %v",
			valuesOn(publisher, serviceOf(envId, "total")))
	}
	for _, value := range valuesOn(publisher, serviceOf(envId, "total")) {
		if _, ok := value.(float64); !ok {
			t.Fatalf("expected a bare number, got %T (%v)", value, value)
		}
	}
}

// ---------------------------------------------------------------------------
// the index itself
// ---------------------------------------------------------------------------

// The index is what the running sum reads, and it is built once per generation.
// Testing it directly pins the parts a timing based test cannot see: the order
// of the inputs, and what is left out.
func TestNewGenerationIndexesAggregateInputs(t *testing.T) {
	const envId = "env-agg-index"
	env := treeEnvironment(envId,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", serviceOf(envId, "total"), 1, energyCharacteristic),
			//a second aggregate on the same asset, matching nothing
			aggregateChannel("ch-other", serviceOf(envId, "other"), 1, otherCharacteristic),
		}},
		treeAsset{id: "a-sub-1", submeteredBy: "a-total", channels: []domain.Channel{
			measuringChannel("ch-sub-1a", serviceOf(envId, "sub-1a"), energyCharacteristic, 1),
			measuringChannel("ch-sub-1b", serviceOf(envId, "sub-1b"), energyCharacteristic, 2),
			measuringChannel("ch-sub-1c", serviceOf(envId, "sub-1c"), otherCharacteristic, 3),
		}},
		treeAsset{id: "a-sub-2", submeteredBy: "a-total", channels: []domain.Channel{
			measuringChannel("ch-sub-2a", serviceOf(envId, "sub-2a"), energyCharacteristic, 4)}},
		treeAsset{id: "a-loose", channels: []domain.Channel{
			measuringChannel("ch-loose", serviceOf(envId, "loose"), energyCharacteristic, 5)}},
	)
	gen := newGeneration(env, nil)

	//document order, matched by characteristic, the asset on the zone left out
	want := []string{"ch-sub-1a", "ch-sub-1b", "ch-sub-2a"}
	if got := gen.aggregateInputs["ch-total"]; !reflect.DeepEqual(got, want) {
		t.Errorf("expected the energy inputs %v in document order, got %v", want, got)
	}
	if got := gen.aggregateInputs["ch-other"]; !reflect.DeepEqual(got, []string{"ch-sub-1c"}) {
		t.Errorf("the second aggregate has to match its own characteristic, got %v", got)
	}
	if gen.candidates != nil {
		t.Error("the indexing scaffolding has to be dropped once the pass is done")
	}
}

// An asset naming itself is refused by validation; a hand written document
// carrying one must not make the aggregate sum its own last value, which would
// double the reading on every tick.
//
// The document below is refused twice over - the measured channel next to the
// aggregate on one asset is the double counting rule (checkAggregateOverlap) -
// which is why this one indexes a generation directly instead of going through
// Validate: the guard has to hold for whatever bypassed the api.
func TestAnAggregateNeverSumsItself(t *testing.T) {
	const envId = "env-agg-self"
	env := treeEnvironment(envId,
		treeAsset{id: "a-total", submeteredBy: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", serviceOf(envId, "total"), 1, energyCharacteristic),
			measuringChannel("ch-own", serviceOf(envId, "own"), energyCharacteristic, 10),
		}},
	)
	gen := newGeneration(env, nil)
	if got := gen.aggregateInputs["ch-total"]; len(got) != 0 {
		t.Errorf("a self reference must not become an input, got %v", got)
	}
}

// Validation demands a characteristic on an aggregate channel. A document that
// bypassed it must not fall back to "match the channels that have none either"
// - a rule nobody wrote down - so it sums nothing and says so in the log.
func TestAnAggregateWithoutACharacteristicSumsNothing(t *testing.T) {
	const envId = "env-agg-nochar"
	env := treeEnvironment(envId,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", serviceOf(envId, "total"), 1, "")}},
		treeAsset{id: "a-sub", submeteredBy: "a-total", channels: []domain.Channel{
			measuringChannel("ch-sub", serviceOf(envId, "sub"), "", 10)}},
	)
	gen := newGeneration(env, nil)
	if got := gen.aggregateInputs["ch-total"]; len(got) != 0 {
		t.Errorf("without a characteristic there is nothing to match by, got %v", got)
	}
	//it stays executable all the same: it publishes zero rather than nothing
	found := false
	for _, binding := range gen.sensors {
		if binding.channel.Id == "ch-total" {
			found = true
		}
	}
	if !found {
		t.Error("the aggregate channel has to keep ticking, publishing zero")
	}
}

// ---------------------------------------------------------------------------
// what survives a reload, and what must not
// ---------------------------------------------------------------------------

// captureLogs collects what the package logger writes during one call and puts
// the previous logger back.
//
// util.Logger is a package variable, so this is only safe around code that runs
// no goroutines of its own - every use below wraps a bare newGeneration, never a
// running runtime.
func captureLogs(t *testing.T, during func()) string {
	t.Helper()
	buffer := &bytes.Buffer{}
	previous := util.Logger
	util.Logger = slog.New(slog.NewTextHandler(buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	defer func() { util.Logger = previous }()
	during()
	return buffer.String()
}

// deadChannel is the edit that made a stale value leak: the channel keeps its id
// and its characteristic, so it stays an input of the aggregate, but it becomes
// an actuator without a platform service - no runner, and no service a command
// could arrive on, so nothing in the new generation can ever produce a value for
// it.
func deadChannel(channel domain.Channel) domain.Channel {
	channel.Direction = domain.Actuator
	channel.IntervalSeconds = 0
	channel.ExternalRef = ""
	channel.Source = domain.Source{Kind: domain.SourceScript, Script: &domain.ScriptSource{Code: "1;"}}
	return channel
}

// reloadWith stores a new definition and reloads the environment, then returns
// the number of totals published up to that point. Everything published from
// that index on comes from the new generation: Reload stops the old runners
// before it starts the new ones.
func reloadWith(t *testing.T, rt *Runtime, envs *fakeEnvironments, publisher *fakePublisher, changed domain.Environment, totalRef string) int {
	t.Helper()
	if err := domain.Validate(changed); err != nil {
		t.Fatalf("the changed document has to be a legal one: %v", err)
	}
	if _, err := envs.Put(context.Background(), changed); err != nil {
		t.Fatalf("unable to store the changed document: %v", err)
	}
	rt.Reload(changed.Id)
	return len(valuesOn(publisher, totalRef))
}

// The environment object, and with it the cache of last published values,
// survives a reload; the generation does not. A channel that can no longer
// produce a value in the new generation must not keep contributing the value it
// had in the old one - it did, and it was invisible, because a frozen 10 kWh
// looks exactly like a meter that is still reading 10 kWh.
func TestAnAggregateDropsWhatAChannelCanNoLongerProduceAfterAReload(t *testing.T) {
	const envId = "env-agg-reload-dead"
	totalRef := serviceOf(envId, "total")
	child := measuringChannel("ch-sub", serviceOf(envId, "sub"), energyCharacteristic, 10)
	total := treeAsset{id: "a-total", channels: []domain.Channel{
		aggregateChannel("ch-total", totalRef, 1, energyCharacteristic)}}
	envs := newFakeEnvironments(treeEnvironment(envId, total,
		treeAsset{id: "a-sub", submeteredBy: "a-total", channels: []domain.Channel{child}}))
	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(time.Hour), envs, newFakeStates(), publisher)

	if !waitFor(8*time.Second, func() bool { return sawValue(publisher, totalRef, 10.0) }) {
		t.Fatalf("setup: the total never reached 10, it published %v", valuesOn(publisher, totalRef))
	}

	before := reloadWith(t, rt, envs, publisher, treeEnvironment(envId, total,
		treeAsset{id: "a-sub", submeteredBy: "a-total", channels: []domain.Channel{deadChannel(child)}}), totalRef)

	if !waitFor(8*time.Second, func() bool { return len(valuesOn(publisher, totalRef)) >= before+3 }) {
		t.Fatalf("the aggregate stopped publishing after the reload: %v", valuesOn(publisher, totalRef))
	}
	for _, value := range valuesOn(publisher, totalRef)[before:] {
		if value != 0.0 {
			t.Fatalf("a channel nothing can drive any more still contributed to the total: %v after the reload",
				valuesOn(publisher, totalRef)[before:])
		}
	}
}

// The other side of the same decision, and the reason the prune keeps command
// bindings: an actuator is driven from outside, and the value it last produced
// is the value it still has. Dropping it would make every reload dent the total
// above it until the next command happens to arrive.
func TestAnAggregateKeepsWhatACommandCanStillDriveAcrossAReload(t *testing.T) {
	const envId = "env-agg-reload-commanded"
	totalRef := serviceOf(envId, "total")
	child := measuringChannel("ch-sub", serviceOf(envId, "sub"), energyCharacteristic, 10)
	total := treeAsset{id: "a-total", channels: []domain.Channel{
		aggregateChannel("ch-total", totalRef, 1, energyCharacteristic)}}
	envs := newFakeEnvironments(treeEnvironment(envId, total,
		treeAsset{id: "a-sub", submeteredBy: "a-total", channels: []domain.Channel{child}}))
	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(time.Hour), envs, newFakeStates(), publisher)

	if !waitFor(8*time.Second, func() bool { return sawValue(publisher, totalRef, 10.0) }) {
		t.Fatalf("setup: the total never reached 10, it published %v", valuesOn(publisher, totalRef))
	}

	//the same edit as above, except that the platform service stays: the channel
	//has no runner, but a command can reach it
	commanded := deadChannel(child)
	commanded.ExternalRef = serviceOf(envId, "sub")
	before := reloadWith(t, rt, envs, publisher, treeEnvironment(envId, total,
		treeAsset{id: "a-sub", submeteredBy: "a-total", channels: []domain.Channel{commanded}}), totalRef)

	if !waitFor(8*time.Second, func() bool { return len(valuesOn(publisher, totalRef)) >= before+3 }) {
		t.Fatalf("the aggregate stopped publishing after the reload: %v", valuesOn(publisher, totalRef))
	}
	for _, value := range valuesOn(publisher, totalRef)[before:] {
		if value != 10.0 {
			t.Fatalf("a channel a command can still drive lost its remembered value: %v after the reload",
				valuesOn(publisher, totalRef)[before:])
		}
	}
}

// ---------------------------------------------------------------------------
// a restart of a cumulative chain
// ---------------------------------------------------------------------------

// storedCounter is the runtime state a restart finds: one asset, one channel,
// one meter reading.
func storedCounter(envId string, assetId string, channelId string, reading float64) repo.RuntimeState {
	return repo.RuntimeState{
		EnvironmentId: envId,
		Context:       map[string]interface{}{},
		Zones:         map[string]map[string]interface{}{},
		Assets:        map[string]map[string]interface{}{assetId: {channelId: reading}},
	}
}

// A cumulative meter's reading is persisted state, and the value the channel
// last published is that same reading. Without seeding the cache from it, the
// total starts the process at 0 and steps up to 5000 once the child has ticked -
// a zero phase and a jump in a series whose whole point is that it only rises.
func TestAnAggregateStartsFromThePersistedCounterOfACumulativeChild(t *testing.T) {
	const envId = "env-agg-cumulative-restart"
	totalRef := serviceOf(envId, "total")
	//base 3600 per hour with a one second tick: one unit per tick, so the
	//counter moves visibly and never falls back below the stored reading
	child := profileChannel("ch-sub", serviceOf(envId, "sub"), 1, domain.ProfileSource{Base: 3600, Cumulative: true})
	child.CharacteristicId = energyCharacteristic
	env := treeEnvironment(envId,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", totalRef, 1, energyCharacteristic)}},
		treeAsset{id: "a-sub", submeteredBy: "a-total", channels: []domain.Channel{child}},
	)
	if err := domain.Validate(env); err != nil {
		t.Fatalf("the document has to be a legal one: %v", err)
	}
	states := newFakeStates()
	states.stored[envId] = storedCounter(envId, "a-sub", "ch-sub", 5000)
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), states, publisher)

	if !waitFor(8*time.Second, func() bool { return len(valuesOn(publisher, totalRef)) >= 3 }) {
		t.Fatalf("the aggregate did not publish, it published %v", valuesOn(publisher, totalRef))
	}
	for i, value := range valuesOn(publisher, totalRef) {
		number, ok := value.(float64)
		if !ok {
			t.Fatalf("expected a bare number, got %T (%v)", value, value)
		}
		if number < 5000 {
			t.Errorf("total %d is %v, below the 5000 the only child's meter already stands at: the restart started the sum at zero",
				i, number)
		}
	}
}

// ---------------------------------------------------------------------------
// a value that is not a number
// ---------------------------------------------------------------------------

// checkStates refuses NaN and infinity for stored states, but nothing stops a
// script from sending 1/0 on a channel. Carrying that into the sum would turn
// the total of every level above it into a non-number; one summand missing from
// one total is the smaller loss.
func TestAnAggregateLeavesOutAChildThatSendsInfinity(t *testing.T) {
	const envId = "env-agg-infinite"
	totalRef := serviceOf(envId, "total")
	badRef := serviceOf(envId, "bad")
	bad := scriptChannel("ch-bad", domain.Sensor, 1, badRef, "moses.service.send(1/0);")
	bad.CharacteristicId = energyCharacteristic
	env := treeEnvironment(envId,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", totalRef, 1, energyCharacteristic)}},
		treeAsset{id: "a-bad", submeteredBy: "a-total", channels: []domain.Channel{bad}},
		treeAsset{id: "a-good", submeteredBy: "a-total", channels: []domain.Channel{
			measuringChannel("ch-good", serviceOf(envId, "good"), energyCharacteristic, 4)}},
	)
	if err := domain.Validate(env); err != nil {
		t.Fatalf("the document has to be a legal one: %v", err)
	}
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	//the premise of the test: the script really does produce a non-finite
	//number. Without it the assertion below would pass for the wrong reason.
	if !waitFor(8*time.Second, func() bool {
		for _, value := range valuesOn(publisher, badRef) {
			if number, ok := value.(float64); ok && math.IsInf(number, 0) {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("the script did not publish an infinite value, it published %v", valuesOn(publisher, badRef))
	}
	//from here on the infinity is in the cache. The total published in the same
	//second may have been summed before it arrived, so the assertion needs a few
	//totals that were certainly computed afterwards - without them this test
	//passes whether the sum skips the infinity or not.
	mark := len(valuesOn(publisher, totalRef))
	if !waitFor(8*time.Second, func() bool { return len(valuesOn(publisher, totalRef)) >= mark+3 }) {
		t.Fatalf("the aggregate stopped publishing, it published %v", valuesOn(publisher, totalRef))
	}
	if !sawValue(publisher, totalRef, 4.0) {
		t.Errorf("the total never reached the value of its finite child, it published %v", valuesOn(publisher, totalRef))
	}
	//anything but 0 and 4 here is the infinity having reached the total: +Inf
	//itself, or a NaN from adding it to something
	assertOnlyValues(t, publisher, totalRef, 0, 4)
}

// ---------------------------------------------------------------------------
// matching characteristics that only look different
// ---------------------------------------------------------------------------

// Validation only refuses a characteristic that is empty after trimming, so
// "kwh " is storable and is the same characteristic as "kwh" to everybody
// reading the document. Compared raw, a trailing space made the aggregate match
// nothing and publish a plausible 0 forever.
func TestAnAggregateMatchesACharacteristicWithSurroundingWhitespace(t *testing.T) {
	const envId = "env-agg-whitespace"
	env := treeEnvironment(envId,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", serviceOf(envId, "total"), 1, energyCharacteristic+" ")}},
		treeAsset{id: "a-sub", submeteredBy: "a-total", channels: []domain.Channel{
			measuringChannel("ch-sub", serviceOf(envId, "sub"), " "+energyCharacteristic, 10)}},
	)
	if err := domain.Validate(env); err != nil {
		t.Fatalf("a characteristic with surrounding whitespace is storable, so this document has to validate: %v", err)
	}
	gen := newGeneration(env, nil)
	if got := gen.aggregateInputs["ch-total"]; !reflect.DeepEqual(got, []string{"ch-sub"}) {
		t.Errorf("expected the child to be matched across the whitespace, got %v", got)
	}
}

// A tree that is there and a sum that stays empty is the shape of a document
// migrated from the legacy format, where the channels carry no characteristic at
// all (lib/repo/convert.go). It publishes a plausible 0 forever, so the index
// pass says so.
func TestAnAggregateWithChildrenAndNoMatchSaysSo(t *testing.T) {
	const envId = "env-agg-nomatch"
	env := treeEnvironment(envId,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", serviceOf(envId, "total"), 1, energyCharacteristic)}},
		treeAsset{id: "a-sub", submeteredBy: "a-total", channels: []domain.Channel{
			measuringChannel("ch-sub", serviceOf(envId, "sub"), "", 10)}},
	)
	var gen *generation
	logs := captureLogs(t, func() { gen = newGeneration(env, nil) })

	if got := gen.aggregateInputs["ch-total"]; len(got) != 0 {
		t.Errorf("a child without the characteristic must not be summed, got %v", got)
	}
	for _, fragment := range []string{"sums nothing", "ch-total", energyCharacteristic, "submetered_assets=1"} {
		if !bytes.Contains([]byte(logs), []byte(fragment)) {
			t.Errorf("the warning has to name %q, it was: %s", fragment, logs)
		}
	}
}

// The complement: an aggregate whose children do match must not be reported as
// summing nothing, or the line above becomes noise nobody reads.
func TestAnAggregateThatMatchesIsNotReportedAsEmpty(t *testing.T) {
	const envId = "env-agg-match-quiet"
	env := treeEnvironment(envId,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", serviceOf(envId, "total"), 1, energyCharacteristic)}},
		treeAsset{id: "a-sub", submeteredBy: "a-total", channels: []domain.Channel{
			measuringChannel("ch-sub", serviceOf(envId, "sub"), energyCharacteristic, 10)}},
	)
	logs := captureLogs(t, func() { newGeneration(env, nil) })
	if bytes.Contains([]byte(logs), []byte("sums nothing")) {
		t.Errorf("a matching aggregate must not be reported as summing nothing: %s", logs)
	}
}

// ---------------------------------------------------------------------------
// an input that has not produced a value yet
// ---------------------------------------------------------------------------

// A channel that has produced nothing yet is unknown, not zero. Summed as zero
// the total is short by a whole sub-meter, and a total that is too low is
// indistinguishable from a real drop - so nothing goes out until every input
// this generation can tick has produced once.
//
// The script child publishes every four seconds, the total every second, so the
// first three totals would be the profile child alone.
func TestAnAggregateWaitsForAnInputThatHasNotProducedYet(t *testing.T) {
	const envId = "env-agg-await-script"
	totalRef := serviceOf(envId, "total")
	slow := scriptChannel("ch-slow", domain.Sensor, 4, serviceOf(envId, "slow"), "moses.service.send(100);")
	slow.CharacteristicId = energyCharacteristic
	env := treeEnvironment(envId,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", totalRef, 1, energyCharacteristic)}},
		treeAsset{id: "a-fast", submeteredBy: "a-total", channels: []domain.Channel{
			measuringChannel("ch-fast", serviceOf(envId, "fast"), energyCharacteristic, 10)}},
		treeAsset{id: "a-slow", submeteredBy: "a-total", channels: []domain.Channel{slow}},
	)
	if err := domain.Validate(env); err != nil {
		t.Fatalf("the document has to be a legal one: %v", err)
	}
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	//the premise: the fast child really is publishing while the slow one has not
	//run yet, so the total had every chance to go out short
	if !waitFor(8*time.Second, func() bool { return len(valuesOn(publisher, serviceOf(envId, "fast"))) >= 2 }) {
		t.Fatalf("setup: the fast child did not publish, it published %v", valuesOn(publisher, serviceOf(envId, "fast")))
	}
	if len(valuesOn(publisher, serviceOf(envId, "slow"))) > 0 {
		t.Fatalf("setup: the slow child was expected to be silent still, it published %v",
			valuesOn(publisher, serviceOf(envId, "slow")))
	}
	if got := valuesOn(publisher, totalRef); len(got) != 0 {
		t.Fatalf("the total must publish nothing at all while an input has produced no value, it published %v", got)
	}

	if !waitFor(10*time.Second, func() bool { return sawValue(publisher, totalRef, 110.0) }) {
		t.Fatalf("the total never reached 10+100=110 once both children had produced, it published %v",
			valuesOn(publisher, totalRef))
	}
	//10 would be the profile child alone, 100 the script child alone, 0 neither
	assertOnlyValues(t, publisher, totalRef, 110)
}

// An input nothing in this generation can tick is not waited for: it contributes
// 0 for as long as the generation runs (indexAggregates says so in the log), and
// waiting for it would silence the total forever instead. The end to end case is
// TestAnAggregateDropsWhatAChannelCanNoLongerProduceAfterAReload; this pins the
// index the rule reads.
func TestOnlyInputsWithARunnerAreAwaited(t *testing.T) {
	const envId = "env-agg-awaited-index"
	//no publish interval, no source interval and no platform service: nothing
	//ticks it and no command can reach it
	dead := deadChannel(measuringChannel("ch-dead", "", energyCharacteristic, 10))
	env := treeEnvironment(envId,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", serviceOf(envId, "total"), 1, energyCharacteristic)}},
		treeAsset{id: "a-live", submeteredBy: "a-total", channels: []domain.Channel{
			measuringChannel("ch-live", serviceOf(envId, "live"), energyCharacteristic, 10)}},
		treeAsset{id: "a-dead", submeteredBy: "a-total", channels: []domain.Channel{dead}},
	)
	gen := newGeneration(env, nil)

	if got := gen.aggregateInputs["ch-total"]; !reflect.DeepEqual(got, []string{"ch-live", "ch-dead"}) {
		t.Errorf("both children stay inputs of the sum, got %v", got)
	}
	if got := gen.aggregateAwaited["ch-total"]; !reflect.DeepEqual(got, []string{"ch-live"}) {
		t.Errorf("only the child with a runner may be waited for, got %v", got)
	}
}

// The defect this rule exists for: lastValues is in memory only and
// carryLastValues restores the counters of cumulative profiles alone, so after
// a restart a script or formula child reads as a missing map entry. Summed as
// 0, the first total after the restart was the cumulative child alone - a drop
// of the whole script child's share on a distribution meter, published as a
// plausible reading.
func TestTheFirstTotalAfterARestartIsNotSmallerThanTheLastOneBefore(t *testing.T) {
	const envId = "env-agg-restart-script"
	totalRef := serviceOf(envId, "total")
	//base 3600 with a one second tick: the meter climbs by one per tick
	counter := profileChannel("ch-counter", serviceOf(envId, "counter"), 1, domain.ProfileSource{Base: 3600, Cumulative: true})
	counter.CharacteristicId = energyCharacteristic
	//four seconds against the total's one: after the restart the total ticks
	//three times before the script has produced anything, which is what makes
	//the assertion below independent of the order two runners happen to start in
	script := scriptChannel("ch-script", domain.Sensor, 4, serviceOf(envId, "script"), "moses.service.send(1000);")
	script.CharacteristicId = energyCharacteristic
	env := treeEnvironment(envId,
		treeAsset{id: "a-total", channels: []domain.Channel{
			aggregateChannel("ch-total", totalRef, 1, energyCharacteristic)}},
		treeAsset{id: "a-counter", submeteredBy: "a-total", channels: []domain.Channel{counter}},
		treeAsset{id: "a-script", submeteredBy: "a-total", channels: []domain.Channel{script}},
	)
	if err := domain.Validate(env); err != nil {
		t.Fatalf("the document has to be a legal one: %v", err)
	}
	envs := newFakeEnvironments(env)
	states := newFakeStates()
	publisher := &fakePublisher{}
	first := startRuntime(t, testConfig(time.Hour), envs, states, publisher)

	//waited for a total that carries the script child's share: a restart is only
	//a regression against a total that was complete to begin with
	if !waitFor(15*time.Second, func() bool {
		got := valuesOn(publisher, totalRef)
		if len(got) < 2 {
			return false
		}
		number, ok := got[len(got)-1].(float64)
		return ok && number >= 1000
	}) {
		t.Fatalf("setup: the total never carried the script child's 1000 before the restart, it published %v",
			valuesOn(publisher, totalRef))
	}
	//Stop flushes, which is what puts the meter reading into the store the
	//second incarnation loads
	first.Stop()

	before := valuesOn(publisher, totalRef)
	last, ok := before[len(before)-1].(float64)
	if !ok {
		t.Fatalf("expected a bare number, got %T (%v)", before[len(before)-1], before[len(before)-1])
	}
	mark := len(before)

	startRuntime(t, testConfig(time.Hour), envs, states, publisher)
	if !waitFor(15*time.Second, func() bool { return len(valuesOn(publisher, totalRef)) > mark }) {
		t.Fatalf("the total never published again after the restart, it published %v", valuesOn(publisher, totalRef))
	}
	//every total of the second incarnation, not only the first: the counter only
	//ever rises, so none of them may fall below the last one of the first
	for i, value := range valuesOn(publisher, totalRef)[mark:] {
		number, ok := value.(float64)
		if !ok {
			t.Fatalf("expected a bare number, got %T (%v)", value, value)
		}
		if number < last {
			t.Errorf("total %d after the restart is %v, below the %v published before it: the restart summed a child that had produced nothing as 0",
				i, number, last)
		}
	}
}
