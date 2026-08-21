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
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/state"
)

// The acceptance criterion of the migration is not that the routines are
// attached but that the converted environment still simulates. This runs the
// real Industry scripts, converted, against the new runtime.
//
// The fixture is read out of lib/domain rather than copied: it is the export of
// the one production world, and two copies would drift.
const industryFixture = "../domain/testdata/industry-world.json"

// speedUp rewrites the intervals so the test finishes in seconds. The ratio is
// what matters here - the source runs more often than the channel publishes -
// and the interval arithmetic itself is pinned in lib/domain.
func speedUp(env domain.Environment, sourceSeconds int64, publishSeconds int64) domain.Environment {
	for z := range env.Zones {
		for a := range env.Zones[z].Assets {
			for c := range env.Zones[z].Assets[a].Channels {
				channel := &env.Zones[z].Assets[a].Channels[c]
				if channel.Source.IntervalSeconds > 0 {
					channel.Source.IntervalSeconds = sourceSeconds
				}
				if channel.IntervalSeconds > 0 {
					channel.IntervalSeconds = publishSeconds
				}
			}
		}
	}
	return env
}

func industryEnvironment(t *testing.T) domain.Environment {
	t.Helper()
	content, err := os.ReadFile(industryFixture)
	if err != nil {
		t.Fatal(err)
	}
	world := state.World{}
	if err = json.Unmarshal(content, &world); err != nil {
		t.Fatal(err)
	}
	world.Owner = "owner-1"
	env, problems, err := domain.FromLegacyWorld(world, domain.IndustrialSite)
	if err != nil {
		t.Fatal(err)
	}
	//the three room routines are reported and not migrated, by decision
	if len(problems) != 3 {
		t.Fatalf("expected only the room routines to be reported, got %v", problems)
	}
	return env
}

func TestTheConvertedIndustryWorldStillSimulates(t *testing.T) {
	env := speedUp(industryEnvironment(t), 1, 2)
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	//two publish intervals plus slack, so a channel can produce two values
	if !waitFor(20*time.Second, func() bool { return varyingChannels(publisher) > 0 }) {
		t.Fatalf("no channel produced two different values in 20s, so the physics is not running:\n%v",
			summarise(publisher))
	}

	//every sensor channel of the export has to be publishing, not just the ones
	//that carry a routine
	publishing := map[string]bool{}
	for _, event := range publisher.all() {
		publishing[event.deviceRef+"|"+event.serviceRef] = true
	}
	if len(publishing) != 24 {
		t.Errorf("expected all 24 channels to publish, got %d:\n%v", len(publishing), summarise(publisher))
	}
	t.Logf("channels publishing: %d, of which varying: %d", len(publishing), varyingChannels(publisher))
}

// varyingChannels counts the channels that published two values that differ. A
// channel whose value never moves is a channel whose physics did not run.
func varyingChannels(publisher *fakePublisher) int {
	seen := map[string]map[string]bool{}
	for _, event := range publisher.all() {
		key := event.deviceRef + "|" + event.serviceRef
		if seen[key] == nil {
			seen[key] = map[string]bool{}
		}
		seen[key][fmt.Sprint(event.value)] = true
	}
	count := 0
	for _, values := range seen {
		if len(values) > 1 {
			count++
		}
	}
	return count
}

func summarise(publisher *fakePublisher) map[string]int {
	result := map[string]int{}
	for _, event := range publisher.all() {
		result[event.serviceRef]++
	}
	return result
}
