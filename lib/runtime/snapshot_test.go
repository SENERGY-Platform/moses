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
	"math"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

func TestSnapshotReportsAnEnvironmentThatIsNotRunning(t *testing.T) {
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), &fakePublisher{})
	_, err := rt.Snapshot("nope")
	if !errors.Is(err, repo.ErrNotRunning) {
		//an empty snapshot would be indistinguishable from a running environment
		//that has not written anything yet
		t.Errorf("expected ErrNotRunning, got %v", err)
	}
}

func TestSnapshotReturnsTheLiveContextZoneAndAssetStates(t *testing.T) {
	env := testEnvironment("env-read", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-read"), readsZoneTemperature))
	env.Context = map[string]interface{}{"outdoor": 5.0}
	env.Zones[0].InitialStates = map[string]interface{}{"temperature": 18.0}
	env.Zones[0].Assets[0].InitialStates = map[string]interface{}{"rpm": 1450.0}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), &fakePublisher{})

	got, err := rt.Snapshot("env-read")
	if err != nil {
		t.Fatal(err)
	}
	if got.State.Context["outdoor"] != 5.0 {
		t.Errorf("expected the context to be readable, got %#v", got.State.Context)
	}
	if got.State.Zones[testZoneId]["temperature"] != 18.0 {
		t.Errorf("expected the zone state to be readable, got %#v", got.State.Zones)
	}
	if got.State.Assets[testAssetId]["rpm"] != 1450.0 {
		t.Errorf("expected the asset state to be readable, got %#v", got.State.Assets)
	}
	if time.Since(got.AsOf) > time.Minute || got.AsOf.IsZero() {
		t.Errorf("expected as_of to be the moment of the read, got %v", got.AsOf)
	}
}

// The one that would go unnoticed until a caller changed something in place: the
// snapshot must not hand out the maps the scripts keep writing into.
func TestSnapshotHandsOutCopiesAndNotTheLiveMaps(t *testing.T) {
	env := testEnvironment("env-copy", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-copy"), readsZoneTemperature))
	env.Context = map[string]interface{}{
		"outdoor": 5.0,
		"nested":  map[string]interface{}{"inner": 1.0},
		"list":    []interface{}{1.0, 2.0},
	}
	env.Zones[0].InitialStates = map[string]interface{}{"temperature": 18.0}
	env.Zones[0].Assets[0].InitialStates = map[string]interface{}{"rpm": 1450.0}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), &fakePublisher{})

	got, err := rt.Snapshot("env-copy")
	if err != nil {
		t.Fatal(err)
	}
	//every level: the maps themselves, a nested map and a slice inside a value
	got.State.Context["outdoor"] = -99.0
	got.State.Context["nested"].(map[string]interface{})["inner"] = -99.0
	got.State.Context["list"].([]interface{})[0] = -99.0
	got.State.Zones[testZoneId]["temperature"] = -99.0
	got.State.Assets[testAssetId]["rpm"] = -99.0
	delete(got.State.Zones, testZoneId)

	again, err := rt.Snapshot("env-copy")
	if err != nil {
		t.Fatal(err)
	}
	if again.State.Context["outdoor"] != 5.0 {
		t.Errorf("a write to the snapshot changed the live context, got %#v", again.State.Context["outdoor"])
	}
	if again.State.Context["nested"].(map[string]interface{})["inner"] != 1.0 {
		t.Errorf("a write to a nested map of the snapshot reached the live state, got %#v", again.State.Context["nested"])
	}
	if again.State.Context["list"].([]interface{})[0] != 1.0 {
		t.Errorf("a write into a list of the snapshot reached the live state, got %#v", again.State.Context["list"])
	}
	if again.State.Zones[testZoneId]["temperature"] != 18.0 {
		t.Errorf("a write to the snapshot changed the live zone state, got %#v", again.State.Zones)
	}
	if again.State.Assets[testAssetId]["rpm"] != 1450.0 {
		t.Errorf("a write to the snapshot changed the live asset state, got %#v", again.State.Assets)
	}
}

// A zone value with a time constant is only written when it is read. Reading it
// through the api has to advance it the same way a script read does, or the api
// would report the value from whenever a script last happened to look.
func TestSnapshotResolvesAZoneValueWithATimeConstantToTheMomentOfTheRead(t *testing.T) {
	env := testEnvironment("env-lag-read", scriptChannel("ch-1", domain.Sensor, 3600, serviceRefOf("env-lag-read"), readsZoneTemperature))
	env.Zones[0].InitialStates = map[string]interface{}{"temperature": 18.0}
	env.Zones[0].TimeConstants = map[string]int64{"temperature": 100}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), &fakePublisher{})

	//the channel ticks once an hour, so nothing but the snapshot reads the zone
	if err := rt.SetState("env-lag-read", repo.StateChange{
		Zones: map[string]map[string]interface{}{testZoneId: {"temperature": 28.0}},
	}); err != nil {
		t.Fatal(err)
	}

	//the approach was started a moment ago, so an unadvanced read still says 18
	got, err := rt.Snapshot("env-lag-read")
	if err != nil {
		t.Fatal(err)
	}
	if got.State.Zones[testZoneId]["temperature"] != 18.0 {
		t.Fatalf("expected the value to start at 18, got %#v", got.State.Zones[testZoneId]["temperature"])
	}

	//now move the approach's start into the past by one time constant, which is
	//the only way to make an exponential over 100 seconds observable in a test.
	//The law itself is pinned by approach_test.go; what is pinned here is that a
	//snapshot evaluates it at all, and at the instant it reports.
	rt.mux.RLock()
	live := rt.envs["env-lag-read"]
	rt.mux.RUnlock()
	live.mux.Lock()
	approach := live.state.Approaching[testZoneId]["temperature"]
	approach.StartUnix -= approach.TauSeconds
	live.state.Approaching[testZoneId]["temperature"] = approach
	live.mux.Unlock()

	got, err = rt.Snapshot("env-lag-read")
	if err != nil {
		t.Fatal(err)
	}
	value, ok := asFloat(got.State.Zones[testZoneId]["temperature"])
	if !ok {
		t.Fatalf("expected a number, got %#v", got.State.Zones[testZoneId]["temperature"])
	}
	//after one time constant a first order step response has covered 1 - 1/e
	want := 18 + 10*(1-1/math.E)
	if math.Abs(value-want) > 0.5 {
		t.Errorf("expected the value resolved to the moment of the read, about %v, got %v", want, value)
	}
	//and it is resolved to the instant the answer names, not to some earlier one
	if time.Since(got.AsOf) > time.Minute {
		t.Errorf("expected as_of to be the moment of the read, got %v", got.AsOf)
	}
}

// Setting a value and reading it back is the loop the editor closes.
func TestSetStateThenSnapshotShowsTheValueThatWasSet(t *testing.T) {
	env := testEnvironment("env-roundtrip", scriptChannel("ch-1", domain.Sensor, 3600, serviceRefOf("env-roundtrip"), readsZoneTemperature))
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), &fakePublisher{})

	change := repo.StateChange{
		Context: map[string]interface{}{"outdoor": -7.5},
		Zones:   map[string]map[string]interface{}{testZoneId: {"temperature": 26.5}},
		Assets:  map[string]map[string]interface{}{testAssetId: {"rpm": 1450.0}},
	}
	if err := rt.SetState("env-roundtrip", change); err != nil {
		t.Fatal(err)
	}

	got, err := rt.Snapshot("env-roundtrip")
	if err != nil {
		t.Fatal(err)
	}
	if got.State.Context["outdoor"] != -7.5 {
		t.Errorf("expected the context that was set, got %#v", got.State.Context["outdoor"])
	}
	if got.State.Zones[testZoneId]["temperature"] != 26.5 {
		t.Errorf("expected the zone value that was set, got %#v", got.State.Zones[testZoneId]["temperature"])
	}
	if got.State.Assets[testAssetId]["rpm"] != 1450.0 {
		t.Errorf("expected the asset value that was set, got %#v", got.State.Assets[testAssetId]["rpm"])
	}
}

// The snapshot takes the same mutex every script run takes, so this is what
// proves it: readers and ticking channels at the same time, under -race.
func TestSnapshotIsSafeWhileTheEnvironmentTicks(t *testing.T) {
	counting := `moses.room.state.set("n", moses.room.state.get("n") + 1);
		moses.device.state.set("m", moses.device.state.get("m") + 1);
		moses.world.state.set("k", moses.world.state.get("k") + 1);
		moses.service.send(moses.room.state.get("n"));`
	env := testEnvironment("env-race",
		scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-race"), counting),
		scriptChannel("ch-2", domain.Sensor, 1, serviceRefOf("env-race")+"-2", counting))
	env.Zones[0].TimeConstants = map[string]int64{"temperature": 60}
	rt := startRuntime(t, testConfig(20*time.Millisecond), newFakeEnvironments(env), newFakeStates(), &fakePublisher{})

	//an approach in flight, so the readers also write through advanceZone
	if err := rt.SetState("env-race", repo.StateChange{
		Zones: map[string]map[string]interface{}{testZoneId: {"temperature": 28.0}},
	}); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	wg := sync.WaitGroup{}
	failures := make(chan error, 8)
	for reader := 0; reader < 8; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, err := rt.Snapshot("env-race")
				if err != nil {
					failures <- err
					return
				}
				//write into every level of the result: a map still shared with
				//the live state would be a race the detector sees
				for _, values := range got.State.Zones {
					for key := range values {
						values[key] = 0
					}
				}
				for key := range got.State.Context {
					got.State.Context[key] = 0
				}
			}
		}()
	}
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("a concurrent snapshot failed: %v", err)
	}
}

// A removed environment is gone for a reader too, and not an empty state.
func TestSnapshotReportsARemovedEnvironmentAsNotRunning(t *testing.T) {
	env := testEnvironment("env-removed", scriptChannel("ch-1", domain.Sensor, 3600, serviceRefOf("env-removed"), readsZoneTemperature))
	envs := newFakeEnvironments(env)
	rt := startRuntime(t, testConfig(time.Hour), envs, newFakeStates(), &fakePublisher{})
	if _, err := rt.Snapshot("env-removed"); err != nil {
		t.Fatal(err)
	}

	if err := envs.Delete(t.Context(), "env-removed"); err != nil {
		t.Fatal(err)
	}
	rt.Remove("env-removed")

	if _, err := rt.Snapshot("env-removed"); !errors.Is(err, repo.ErrNotRunning) {
		t.Errorf("expected ErrNotRunning after a remove, got %v", err)
	}
}
