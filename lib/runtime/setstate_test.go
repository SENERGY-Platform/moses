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
	"errors"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// readsZoneTemperature is the shape a room climate sensor takes: the asset does
// not compute the temperature, it reports the one its zone has.
const readsZoneTemperature = `moses.service.send(moses.room.state.get("temperature"));`

// TestSettingAZoneTemperatureReachesTheSensorsInThatZone is the requirement in
// one test: a temperature is set from outside and the simulated sensors pick it
// up, without anything about the definition changing.
func TestSettingAZoneTemperatureReachesTheSensorsInThatZone(t *testing.T) {
	env := testEnvironment("env-climate", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-climate"), readsZoneTemperature))
	env.Zones[0].InitialStates = map[string]interface{}{"temperature": 18.0}
	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("the sensor did not publish the initial zone temperature")
	}
	if got := lastValue(publisher); got != 18.0 {
		t.Fatalf("expected the initial 18, got %v", got)
	}

	err := rt.SetState("env-climate", repo.StateChange{
		Zones: map[string]map[string]interface{}{testZoneId: {"temperature": 26.5}},
	})
	if err != nil {
		t.Fatalf("unable to set the zone temperature: %v", err)
	}

	if !waitFor(4*time.Second, func() bool { return lastValue(publisher) == 26.5 }) {
		t.Fatalf("the sensor never reported the temperature that was set, last value %v", lastValue(publisher))
	}
}

func TestSettingAssetStateReachesTheChannelsOfThatAsset(t *testing.T) {
	env := testEnvironment("env-asset", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-asset"),
		`moses.service.send(moses.device.state.get("rpm"));`))
	env.Zones[0].Assets[0].InitialStates = map[string]interface{}{"rpm": 0.0}
	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("the sensor did not publish")
	}
	if err := rt.SetState("env-asset", repo.StateChange{
		Assets: map[string]map[string]interface{}{testAssetId: {"rpm": 1450.0}},
	}); err != nil {
		t.Fatalf("unable to set the asset state: %v", err)
	}
	if !waitFor(4*time.Second, func() bool { return lastValue(publisher) == 1450.0 }) {
		t.Fatalf("the channel never saw the manipulated asset state, last value %v", lastValue(publisher))
	}
}

// A change is state, not definition: it must not end up in the stored document,
// and it must survive into the state document instead.
func TestAStateChangeIsFlushedAndDoesNotTouchTheDefinition(t *testing.T) {
	env := testEnvironment("env-flush", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-flush"), readsZoneTemperature))
	envs := newFakeEnvironments(env)
	states := newFakeStates()
	rt := startRuntime(t, testConfig(50*time.Millisecond), envs, states, &fakePublisher{})

	if err := rt.SetState("env-flush", repo.StateChange{Context: map[string]interface{}{"outdoor": -7.5}}); err != nil {
		t.Fatalf("unable to set the context: %v", err)
	}

	flushed := func() bool {
		for _, saved := range states.savedFor("env-flush") {
			if saved.state.Context["outdoor"] == -7.5 {
				return true
			}
		}
		return false
	}
	if !waitFor(4*time.Second, flushed) {
		t.Error("the change was never flushed to the state store")
	}
	stored, err := envs.Get(t.Context(), "env-flush")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Context != nil && stored.Context["outdoor"] != nil {
		t.Errorf("a state change must not reach the definition, got %v", stored.Context)
	}
}

func TestSetStateRefusesAnEnvironmentThatIsNotRunning(t *testing.T) {
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), &fakePublisher{})
	err := rt.SetState("nope", repo.StateChange{Context: map[string]interface{}{"a": 1}})
	if !errors.Is(err, repo.ErrNotRunning) {
		t.Errorf("expected ErrNotRunning, got %v", err)
	}
}

// An id nothing reads would look set and have no effect, so it is refused and
// every offending id is named, not only the first.
func TestSetStateNamesEveryUnknownId(t *testing.T) {
	env := testEnvironment("env-ids", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-ids"), readsZoneTemperature))
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), &fakePublisher{})

	err := rt.SetState("env-ids", repo.StateChange{
		Zones:  map[string]map[string]interface{}{"no-zone": {"a": 1}, testZoneId: {"b": 2}},
		Assets: map[string]map[string]interface{}{"no-asset-b": {"a": 1}, "no-asset-a": {"a": 1}},
	})
	unknown := &repo.UnknownIdsError{}
	if !errors.As(err, &unknown) {
		t.Fatalf("expected an UnknownIdsError, got %v", err)
	}
	if len(unknown.Zones) != 1 || unknown.Zones[0] != "no-zone" {
		t.Errorf("expected only no-zone to be reported, got %v", unknown.Zones)
	}
	//sorted, so two runs report the same thing
	if len(unknown.Assets) != 2 || unknown.Assets[0] != "no-asset-a" || unknown.Assets[1] != "no-asset-b" {
		t.Errorf("expected both unknown assets in order, got %v", unknown.Assets)
	}
}

func lastValue(publisher *fakePublisher) interface{} {
	events := publisher.all()
	if len(events) == 0 {
		return nil
	}
	return events[len(events)-1].value
}
