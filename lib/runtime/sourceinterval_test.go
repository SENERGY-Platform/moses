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

// countingSource increments a state value and sends it, so the published value
// says how often the source ran before it was published.
const countingSource = `
var n = moses.device.state.get("n");
n = (n === undefined || n === null) ? 1 : n + 1;
moses.device.state.set("n", n);
moses.service.send(n);
`

// withSourceInterval is what the migration produces: the physics keeps its own
// interval while the channel publishes on the legacy sensor interval.
func withSourceInterval(channel domain.Channel, seconds int64) domain.Channel {
	channel.Source.IntervalSeconds = seconds
	return channel
}

// TestASourceRunsOnItsOwnIntervalAndPublishesOnTheChannelInterval is the reason
// the source interval exists: an Industry change routine ticks every 5 seconds
// while its sensor published every 30, and folding the two together would slow
// the simulation down by a factor of six.
func TestASourceRunsOnItsOwnIntervalAndPublishesOnTheChannelInterval(t *testing.T) {
	channel := withSourceInterval(scriptChannel("ch-1", domain.Sensor, 3, serviceRefOf("env-split"), countingSource), 1)
	env := testEnvironment("env-split", channel)
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(5*time.Second, func() bool { return publisher.count() >= 1 }) {
		t.Fatalf("expected one publish within the publish interval, got %d", publisher.count())
	}
	// just past the first publish and well before the second
	time.Sleep(700 * time.Millisecond)

	if got := publisher.count(); got != 1 {
		t.Errorf("expected exactly one publish in one publish interval, got %d", got)
	}
	values := publisher.forDevice(deviceRefOf("env-split"))
	if len(values) != 1 {
		t.Fatalf("expected one published value, got %v", values)
	}
	// three source ticks fit into the publish interval; scheduling may cost one
	runs, ok := values[0].(float64)
	if !ok {
		t.Fatalf("expected a number, got %T (%v)", values[0], values[0])
	}
	if runs < 2 {
		t.Errorf("expected the source to have run at least twice before publishing, it ran %v times", runs)
	}
}

// TestWithoutASourceIntervalEveryRunPublishes pins the path every stored
// document takes today, so the new field cannot change existing behaviour.
func TestWithoutASourceIntervalEveryRunPublishes(t *testing.T) {
	env := testEnvironment("env-plain", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-plain"), countingSource))
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(5*time.Second, func() bool { return publisher.count() >= 2 }) {
		t.Fatalf("expected at least two publishes, got %d", publisher.count())
	}
	values := publisher.forDevice(deviceRefOf("env-plain"))
	// one run, one publish: the counter and the number of publishes agree
	for i, value := range values {
		runs, ok := value.(float64)
		if !ok {
			t.Fatalf("expected a number, got %T", value)
		}
		if int(runs) != i+1 {
			t.Errorf("publish %d carried %v, expected %d - a run was not published", i+1, runs, i+1)
		}
	}
}

// TestASourceWithoutAPublishIntervalOnlyEvolvesState is the shape an internal
// driver takes: Industry's rpm is written by one routine and read by five
// others. Nothing is published on a schedule.
func TestASourceWithoutAPublishIntervalOnlyEvolvesState(t *testing.T) {
	channel := withSourceInterval(scriptChannel("ch-1", domain.Actuator, 0, serviceRefOf("env-driver"), countingSource), 1)
	env := testEnvironment("env-driver", channel)
	publisher := &fakePublisher{}
	states := newFakeStates()
	startRuntime(t, testConfig(50*time.Millisecond), newFakeEnvironments(env), states, publisher)

	advanced := func() bool {
		for _, saved := range states.savedFor("env-driver") {
			if n, ok := saved.state.Assets[testAssetId]["n"]; ok {
				if value, isNumber := n.(float64); isNumber && value >= 2 {
					return true
				}
			}
		}
		return false
	}
	if !waitFor(5*time.Second, advanced) {
		t.Fatalf("expected the source to have evolved the state at least twice")
	}
	if got := publisher.count(); got != 0 {
		t.Errorf("expected nothing to be published without a publish interval, got %d", got)
	}
}
