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
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// The curve is checked against the closed form rather than against recorded
// numbers: after one time constant a first order step response has covered
// 1 - 1/e of the distance, after three of them 95 percent.
func TestTheApproachFollowsAFirstOrderStepResponse(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	a := repo.Approach{From: 18, Target: 28, StartUnix: start.Unix(), TauSeconds: 100}

	for _, tc := range []struct {
		name    string
		after   time.Duration
		want    float64
		running bool
	}{
		{"at the start it has not moved", 0, 18, true},
		{"after one time constant, 1-1/e of the way", 100 * time.Second, 18 + 10*(1-1/math.E), true},
		{"after three, 95 percent", 300 * time.Second, 18 + 10*(1-math.Exp(-3)), true},
		{"far out it has settled and is finished", 10000 * time.Second, 28, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, running := approachOf(a, start.Add(tc.after))
			if math.Abs(got-tc.want) > 1e-6 {
				t.Errorf("expected %v, got %v", tc.want, got)
			}
			if running != tc.running {
				t.Errorf("expected running=%v, got %v", tc.running, running)
			}
		})
	}
}

// A cooling set point has to fall, not rise: the same law with the sign flipped.
func TestTheApproachWorksDownwardsToo(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	a := repo.Approach{From: 28, Target: 18, StartUnix: start.Unix(), TauSeconds: 100}
	got, _ := approachOf(a, start.Add(100*time.Second))
	want := 28 - 10*(1-1/math.E)
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("expected %v, got %v", want, got)
	}
	if got >= 28 {
		t.Errorf("a falling set point must fall, got %v", got)
	}
}

// A clock that jumped backwards must not push the value away from its target.
func TestTheApproachHoldsItsStartValueForATimeThatWentBackwards(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	a := repo.Approach{From: 18, Target: 28, StartUnix: start.Unix(), TauSeconds: 100}
	got, running := approachOf(a, start.Add(-time.Hour))
	if got != 18 || !running {
		t.Errorf("expected to hold 18 and keep running, got %v running=%v", got, running)
	}
}

func TestAZoneWithATimeConstantApproachesInsteadOfJumping(t *testing.T) {
	env := testEnvironment("env-lag", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-lag"), readsZoneTemperature))
	env.Zones[0].InitialStates = map[string]interface{}{"temperature": 18.0}
	env.Zones[0].TimeConstants = map[string]int64{"temperature": 600}
	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("the sensor did not publish")
	}
	if err := rt.SetState("env-lag", repo.StateChange{
		Zones: map[string]map[string]interface{}{testZoneId: {"temperature": 28.0}},
	}); err != nil {
		t.Fatal(err)
	}

	//with a ten minute time constant, a couple of seconds must not arrive
	moved := func() bool {
		value, ok := asFloat(lastValue(publisher))
		return ok && value > 18.0
	}
	if !waitFor(6*time.Second, moved) {
		t.Fatalf("the temperature never started moving, last value %v", lastValue(publisher))
	}
	value, _ := asFloat(lastValue(publisher))
	if value >= 28.0 {
		t.Errorf("a ten minute time constant must not be reached in seconds, got %v", value)
	}
	if value <= 18.0 {
		t.Errorf("the value has to have moved off its start, got %v", value)
	}
}

// The guard for every document stored so far: no time constant, no lag.
func TestAZoneWithoutATimeConstantIsStillSetAtOnce(t *testing.T) {
	env := testEnvironment("env-nolag", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-nolag"), readsZoneTemperature))
	env.Zones[0].InitialStates = map[string]interface{}{"temperature": 18.0}
	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("the sensor did not publish")
	}
	if err := rt.SetState("env-nolag", repo.StateChange{
		Zones: map[string]map[string]interface{}{testZoneId: {"temperature": 28.0}},
	}); err != nil {
		t.Fatal(err)
	}
	if !waitFor(4*time.Second, func() bool { return lastValue(publisher) == 28.0 }) {
		t.Errorf("without a time constant the value has to arrive at once, last value %v", lastValue(publisher))
	}
}

// Guards the flush of the approach bookkeeping: without it a restart jumps to
// the target instead of resuming the curve, and nothing else notices, because
// the live values keep looking right until the restart happens.
func TestARunningApproachIsFlushedWithTheState(t *testing.T) {
	env := testEnvironment("env-persist", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-persist"), readsZoneTemperature))
	env.Zones[0].InitialStates = map[string]interface{}{"temperature": 18.0}
	env.Zones[0].TimeConstants = map[string]int64{"temperature": 3600}
	states := newFakeStates()
	rt := startRuntime(t, testConfig(50*time.Millisecond), newFakeEnvironments(env), states, &fakePublisher{})

	if err := rt.SetState("env-persist", repo.StateChange{
		Zones: map[string]map[string]interface{}{testZoneId: {"temperature": 28.0}},
	}); err != nil {
		t.Fatal(err)
	}
	flushed := func() bool {
		for _, saved := range states.savedFor("env-persist") {
			if approach, ok := saved.state.Approaching[testZoneId]["temperature"]; ok {
				return approach.Target == 28.0 && approach.TauSeconds == 3600
			}
		}
		return false
	}
	if !waitFor(4*time.Second, flushed) {
		t.Error("the running approach never reached the state store")
	}
}
