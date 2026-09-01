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
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/util"
)

// The live tests below run on whole seconds, because an interval is seconds in
// the document and a ticker of less than one second cannot be expressed. They
// follow the sourceinterval_test pattern: wait for the lower bound, then sleep
// past the next instant something could happen at and assert the exact count.

// steppingSource sends one value per evaluation, in order, and repeats the last
// one forever - so a test decides exactly how far the value moves between two
// evaluations instead of hoping a profile happens to move that far.
func steppingSource(values ...float64) string {
	literals := make([]string, 0, len(values))
	for _, value := range values {
		literals = append(literals, strconv.FormatFloat(value, 'f', -1, 64))
	}
	return fmt.Sprintf(`
var i = moses.device.state.get("i");
i = (i === undefined || i === null) ? 0 : i;
var values = [%s];
moses.device.state.set("i", i + 1);
moses.service.send(values[i < values.length ? i : values.length - 1]);
`, strings.Join(literals, ", "))
}

// changeChannel is a script channel publishing on change: heartbeat seconds on
// the channel, everything else in the trigger.
func changeChannel(envId string, heartbeat int64, trigger domain.ChangeTrigger, values ...float64) domain.Channel {
	channel := scriptChannel("ch-1", domain.Sensor, heartbeat, serviceRefOf(envId), steppingSource(values...))
	channel.PublishOnChange = &trigger
	return channel
}

// everyEvaluation is the trigger the value-driven tests use: computed once a
// second, with only the threshold differing between them.
func everyEvaluation(absolute float64, relative float64) domain.ChangeTrigger {
	return domain.ChangeTrigger{Absolute: absolute, Relative: relative, EvaluateIntervalSeconds: 1}
}

func publishedValues(t *testing.T, publisher *fakePublisher, envId string) []float64 {
	t.Helper()
	result := []float64{}
	for _, value := range publisher.forDevice(deviceRefOf(envId)) {
		//asFloat, not a type assertion: otto exports a whole number as an int64,
		//so a script sending 10 publishes an int64 while a profile publishes a
		//float64 - and the gate reads both through the same asFloat
		number, ok := asFloat(value)
		if !ok {
			t.Fatalf("expected a number, got %T (%v)", value, value)
		}
		result = append(result, number)
	}
	return result
}

func publishedEventsOf(publisher *fakePublisher, envId string) []publishedEvent {
	result := []publishedEvent{}
	for _, event := range publisher.all() {
		if event.deviceRef == deviceRefOf(envId) {
			result = append(result, event)
		}
	}
	return result
}

func assertValues(t *testing.T, got []float64, want ...float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("expected the values %v, got %v", want, got)
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-9 {
			t.Fatalf("expected the values %v, got %v", want, got)
		}
	}
}

// TestAValueOverTheThresholdIsPublishedAtOnce is the point of the feature: the
// step reaches the platform in the evaluation it happened in, not at the end of
// a ten minute interval.
func TestAValueOverTheThresholdIsPublishedAtOnce(t *testing.T) {
	const id = "env-cov-step"
	//a heartbeat far outside the test, so every publish here is the trigger's
	env := testEnvironment(id, changeChannel(id, 30, everyEvaluation(5, 0), 10, 100))
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(6*time.Second, func() bool { return publisher.count() >= 2 }) {
		t.Fatalf("expected the step to be published within two evaluations, got %d", publisher.count())
	}
	//past two further evaluations, which send 100 again and must stay silent
	time.Sleep(2200 * time.Millisecond)
	assertValues(t, publishedValues(t, publisher, id), 10, 100)
}

// TestAValueUnderTheThresholdIsSuppressed is the other half: without it the
// channel is a ticker with extra fields.
func TestAValueUnderTheThresholdIsSuppressed(t *testing.T) {
	const id = "env-cov-quiet"
	env := testEnvironment(id, changeChannel(id, 30, everyEvaluation(5, 0), 10, 12))
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(6*time.Second, func() bool { return publisher.count() >= 1 }) {
		t.Fatal("expected the first value to be published")
	}
	time.Sleep(2200 * time.Millisecond)
	assertValues(t, publishedValues(t, publisher, id), 10)
}

// TestTheHeartbeatSendsTheValueThatDidNotChange: a value that stops moving must
// not make the channel look dead. The heartbeat repeats the last computed value
// rather than skipping, the way the split channel repeats one.
func TestTheHeartbeatSendsTheValueThatDidNotChange(t *testing.T) {
	const id = "env-cov-heartbeat"
	env := testEnvironment(id, changeChannel(id, 2, everyEvaluation(5, 0), 10))
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(10*time.Second, func() bool { return publisher.count() >= 3 }) {
		t.Fatalf("expected the heartbeat to keep sending the unchanged value, got %d publishes", publisher.count())
	}
	for i, value := range publishedValues(t, publisher, id) {
		if value != 10 { //exact: the heartbeat repeats the value, it does not recompute it
			t.Fatalf("publish %d carried %v, expected the unchanged 10", i, value)
		}
	}
}

// TestAChangePublishResetsTheHeartbeat: the gap is the longest silence, not a
// fixed grid. A value that went out because it moved starts the gap again, or a
// channel that just published would publish once more a moment later.
func TestAChangePublishResetsTheHeartbeat(t *testing.T) {
	const id = "env-cov-reset"
	//evaluations at 1s (10, first publish), 2s (100, a change), then 100 forever.
	//The heartbeat is 3s, so the third publish is due 3s after the second one.
	env := testEnvironment(id, changeChannel(id, 3, everyEvaluation(5, 0), 10, 100))
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(12*time.Second, func() bool { return publisher.count() >= 3 }) {
		t.Fatalf("expected a heartbeat after the change publish, got %d publishes", publisher.count())
	}
	events := publishedEventsOf(publisher, id)
	gap := events[2].at.Sub(events[1].at)
	if gap < 2500*time.Millisecond {
		t.Errorf("the heartbeat came %v after the change publish, so it was not reset by it", gap)
	}
}

// TestTheRelativeThresholdIsMeasuredAgainstTheLastPublishedValue: 100 -> 105 ->
// 120 at ten percent. Measured against the last computed value both steps would
// be under the threshold and the ramp would be invisible; measured against the
// last published one the drift accumulates until it crosses.
func TestTheRelativeThresholdIsMeasuredAgainstTheLastPublishedValue(t *testing.T) {
	const id = "env-cov-relative"
	env := testEnvironment(id, changeChannel(id, 30, everyEvaluation(0, 0.1), 100, 105, 120))
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(8*time.Second, func() bool { return publisher.count() >= 2 }) {
		t.Fatalf("expected the accumulated drift to be published, got %v", publishedValues(t, publisher, id))
	}
	time.Sleep(2200 * time.Millisecond)
	assertValues(t, publishedValues(t, publisher, id), 100, 120)
}

// TestEveryDeviationCountsWhenTheLastPublishedValueIsZero: the relative
// threshold multiplies instead of dividing, so a base of zero has no special
// case in the code - every deviation from it is a change. That is the reading a
// meter starting from zero has to produce.
func TestEveryDeviationCountsWhenTheLastPublishedValueIsZero(t *testing.T) {
	const id = "env-cov-zero"
	env := testEnvironment(id, changeChannel(id, 30, everyEvaluation(0, 0.5), 0, 0.5))
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(8*time.Second, func() bool { return publisher.count() >= 2 }) {
		t.Fatalf("expected any deviation from zero to be published, got %v", publishedValues(t, publisher, id))
	}
	assertValues(t, publishedValues(t, publisher, id)[:2], 0, 0.5)
}

// TestTheFirstValueIsAlwaysPublished: with nothing stored there is nothing to
// compare against, and staying silent until the value happens to move would
// leave a fresh environment without a single reading.
func TestTheFirstValueIsAlwaysPublished(t *testing.T) {
	const id = "env-cov-first"
	//a threshold nothing in this test could ever exceed
	env := testEnvironment(id, changeChannel(id, 60, everyEvaluation(1000, 0), 7))
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(6*time.Second, func() bool { return publisher.count() >= 1 }) {
		t.Fatal("expected the first evaluation to publish")
	}
	time.Sleep(2200 * time.Millisecond)
	assertValues(t, publishedValues(t, publisher, id), 7)
}

// TestASourceIntervalIsTheEvaluationCadence: a script that carries its own
// interval is computed on it, and that is when a change is noticed. The channel
// must not fall into the split shape, which would publish on the heartbeat only.
func TestASourceIntervalIsTheEvaluationCadence(t *testing.T) {
	const id = "env-cov-script"
	channel := scriptChannel("ch-1", domain.Sensor, 30, serviceRefOf(id), steppingSource(10, 100))
	channel.Source.IntervalSeconds = 1
	channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 5}
	env := testEnvironment(id, channel)
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(8*time.Second, func() bool { return publisher.count() >= 2 }) {
		t.Fatalf("expected the source interval to drive the evaluation, got %d publishes in 8s of a 30s heartbeat", publisher.count())
	}
	assertValues(t, publishedValues(t, publisher, id)[:2], 10, 100)
}

// TestTheLastPublishedValueIsPersisted: the comparison base has to survive a
// restart, so it has to be written.
func TestTheLastPublishedValueIsPersisted(t *testing.T) {
	const id = "env-cov-persist"
	env := testEnvironment(id, changeChannel(id, 30, everyEvaluation(5, 0), 10, 100))
	publisher := &fakePublisher{}
	states := newFakeStates()
	startRuntime(t, testConfig(50*time.Millisecond), newFakeEnvironments(env), states, publisher)

	before := time.Now().Unix()
	stored := func() bool {
		for _, save := range states.savedFor(id) {
			published, known := save.state.LastPublished["ch-1"]
			if known && published.Value == 100 {
				return true
			}
		}
		return false
	}
	if !waitFor(8*time.Second, stored) {
		t.Fatalf("the last published value was never written, saves: %d", len(states.savedFor(id)))
	}
	//and the timestamp is the moment of the publish, which is what the heartbeat
	//gap is measured from after a restart
	for _, save := range states.savedFor(id) {
		published, known := save.state.LastPublished["ch-1"]
		if !known || published.Value != 100 {
			continue
		}
		if published.AtUnix < before || published.AtUnix > time.Now().Unix() {
			t.Fatalf("the publish was stamped %d, outside [%d, %d]", published.AtUnix, before, time.Now().Unix())
		}
	}
}

// TestARestartDoesNotRepublishTheStoredValue is what the persistence is for: a
// deployment must not produce a burst of transients across a whole site.
func TestARestartDoesNotRepublishTheStoredValue(t *testing.T) {
	const id = "env-cov-restart"
	env := testEnvironment(id, changeChannel(id, 30, everyEvaluation(5, 0), 10))
	states := newFakeStates()
	states.stored[id] = repo.RuntimeState{
		EnvironmentId: id,
		Context:       map[string]interface{}{},
		Zones:         map[string]map[string]interface{}{},
		Assets:        map[string]map[string]interface{}{},
		LastPublished: map[string]repo.PublishedValue{"ch-1": {Value: 10, AtUnix: time.Now().Unix()}},
	}
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), states, publisher)

	//three evaluations of a value that equals the stored one
	time.Sleep(3200 * time.Millisecond)
	if got := publisher.count(); got != 0 {
		t.Errorf("expected the stored value to keep the channel quiet, got %d publishes of %v", got, publishedValues(t, publisher, id))
	}
}

// TestARestartOnlyWaitsTheRestOfTheHeartbeatGap: what was published before the
// restart still stands, so only the remainder of its gap is owed. Waiting a full
// heartbeat after every restart would stretch the longest silence the channel
// promises to nearly twice its interval.
func TestARestartOnlyWaitsTheRestOfTheHeartbeatGap(t *testing.T) {
	const id = "env-cov-gap"
	env := testEnvironment(id, changeChannel(id, 10, everyEvaluation(5, 0), 10))
	states := newFakeStates()
	states.stored[id] = repo.RuntimeState{
		EnvironmentId: id,
		Context:       map[string]interface{}{},
		Zones:         map[string]map[string]interface{}{},
		Assets:        map[string]map[string]interface{}{},
		//eight of the ten seconds are gone, so two are left
		LastPublished: map[string]repo.PublishedValue{"ch-1": {Value: 10, AtUnix: time.Now().Unix() - 8}},
	}
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), states, publisher)

	//well inside the full gap: only the remainder explains a publish this early
	if !waitFor(6*time.Second, func() bool { return publisher.count() >= 1 }) {
		t.Fatal("expected the heartbeat to fire after the rest of the stored gap, not after a full one")
	}
	assertValues(t, publishedValues(t, publisher, id)[:1], 10)
}

// TestAStoredValueOfAChannelWithoutATriggerIsPruned: an entry nothing compares
// against any more would be written out on every flush forever, and would come
// back as a comparison base if the trigger were ever added again.
func TestAStoredValueOfAChannelWithoutATriggerIsPruned(t *testing.T) {
	const id = "env-cov-prune"
	//no trigger, and an interval long enough that the channel never ticks here:
	//the only thing that can make this environment dirty is the prune
	env := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 3600, flatProfile(230, 0)))
	states := newFakeStates()
	states.stored[id] = repo.RuntimeState{
		EnvironmentId: id,
		Context:       map[string]interface{}{},
		Zones:         map[string]map[string]interface{}{},
		Assets:        map[string]map[string]interface{}{},
		LastPublished: map[string]repo.PublishedValue{
			"ch-1":                    {Value: 10, AtUnix: time.Now().Unix()},
			"ch-of-a-past-generation": {Value: 20, AtUnix: time.Now().Unix()},
		},
	}
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(50*time.Millisecond), newFakeEnvironments(env), states, publisher)

	pruned := func() bool {
		saves := states.savedFor(id)
		return len(saves) > 0 && len(saves[len(saves)-1].state.LastPublished) == 0
	}
	if !waitFor(8*time.Second, pruned) {
		t.Fatalf("the stale entries were not pruned, saves: %#v", states.savedFor(id))
	}
}

// TestAFailedPublishIsNotRememberedAndIsTriedAgain: remembering a value the
// platform refused would suppress every retry of it, and the channel would go
// quiet until the value moved again or the heartbeat came.
func TestAFailedPublishIsNotRememberedAndIsTriedAgain(t *testing.T) {
	const id = "env-cov-fail"
	env := testEnvironment(id, changeChannel(id, 30, everyEvaluation(5, 0), 10))
	states := newFakeStates()
	publisher := &fakePublisher{err: errors.New("the platform refused this reading")}
	startRuntime(t, testConfig(50*time.Millisecond), newFakeEnvironments(env), states, publisher)

	//every evaluation tries again, because nothing was remembered
	if !waitFor(8*time.Second, func() bool { return publisher.count() >= 3 }) {
		t.Fatalf("expected the refused value to be tried again, got %d attempts", publisher.count())
	}
	for _, save := range states.savedFor(id) {
		if _, known := save.state.LastPublished["ch-1"]; known {
			t.Fatal("a refused publish was remembered as the last published value")
		}
	}

	//and once the platform takes it, it is remembered and the retries stop
	publisher.failWith(nil)
	remembered := func() bool {
		for _, save := range states.savedFor(id) {
			if published, known := save.state.LastPublished["ch-1"]; known && published.Value == 10 {
				return true
			}
		}
		return false
	}
	if !waitFor(8*time.Second, remembered) {
		t.Fatal("the value was never remembered after the platform accepted it")
	}
}

// TestACumulativeProfileIntegratesOnTheEvaluationCadence is the trap the
// stepSeconds field exists for: the meter advances once per computation, and a
// channel computing ten times per heartbeat that added a whole heartbeat every
// time would count ten times the energy that flowed.
func TestACumulativeProfileIntegratesOnTheEvaluationCadence(t *testing.T) {
	const id = "env-cov-meter"
	//an hourly rate of 3600 makes one second worth exactly 1
	profile := domain.ProfileSource{Base: 3600, Cumulative: true}
	channel := profileChannel("ch-1", serviceRefOf(id), 10, profile)
	channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 0.5, EvaluateIntervalSeconds: 1}
	env := testEnvironment(id, channel)
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(8*time.Second, func() bool { return publisher.count() >= 3 }) {
		t.Fatalf("expected the meter to advance every evaluation, got %d publishes", publisher.count())
	}
	//one second of a 3600/h rate per evaluation: 1, 2, 3 - not 10, 20, 30
	assertValues(t, publishedValues(t, publisher, id)[:3], 1, 2, 3)
}

// TestAnUnusableChangeTriggerFallsBackToThePlainTicker: validation refuses every
// one of these shapes, so a document carrying one bypassed the api. It keeps
// publishing on its interval rather than not at all.
func TestAnUnusableChangeTriggerFallsBackToThePlainTicker(t *testing.T) {
	const id = "env-cov-degraded"
	//no threshold at all: nothing would ever count as a change
	env := testEnvironment(id, changeChannel(id, 1, domain.ChangeTrigger{EvaluateIntervalSeconds: 1}, 10))
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(6*time.Second, func() bool { return publisher.count() >= 2 }) {
		t.Fatalf("expected the channel to keep ticking without a usable trigger, got %d publishes", publisher.count())
	}
}

// dividingSource is how a channel produces a value that is not finite in
// practice: an author divides by an input that turns out to be zero. numerator 1
// gives an infinity, 0 a NaN. skip is how many finite values come first, so a
// test can decide whether the channel already has a comparison base by the time
// the division happens - the two are separate code paths and only one of them is
// the threshold.
func dividingSource(skip int, numerator int) string {
	return fmt.Sprintf(`
var i = moses.device.state.get("i");
i = (i === undefined || i === null) ? 0 : i;
moses.device.state.set("i", i + 1);
moses.service.send(i < %d ? 100 : (%d)/0);
`, skip, numerator)
}

// TestAValueThatIsNotFiniteNeverCountsAsAChange is the loudest failure this
// feature can have: a channel whose value is not a number publishing on every
// single evaluation, forever, at a rate its interval promises it will not
// approach. A ten second evaluation behind a ten minute heartbeat is sixty times
// the load and sixty times the stored rows, for a channel that is broken.
//
// The two halves are separate code paths. With a comparison base the threshold
// decides, and |±Inf - base| is over every finite threshold; without one the
// first-publish bypass decides, and it never asks the threshold at all. Both are
// exercised here, in one runtime, because each case is a heartbeat long.
func TestAValueThatIsNotFiniteNeverCountsAsAChange(t *testing.T) {
	//four evaluations and exactly one heartbeat inside the window below
	const heartbeat = int64(4)
	cases := map[string]struct {
		numerator int
		// finiteFirst is the channel that publishes a real value before it
		// breaks, so the gate has a comparison base.
		finiteFirst bool
	}{
		"infinity without a comparison base":          {1, false},
		"negative infinity without a comparison base": {-1, false},
		"not a number without a comparison base":      {0, false},
		"infinity against a comparison base":          {1, true},
		"not a number against a comparison base":      {0, true},
	}

	idOf := func(name string) string { return "env-cov-nonfinite-" + strings.ReplaceAll(name, " ", "-") }
	envs := []domain.Environment{}
	for name, testCase := range cases {
		id := idOf(name)
		skip := 0
		if testCase.finiteFirst {
			skip = 1
		}
		channel := scriptChannel("ch-1", domain.Sensor, heartbeat, serviceRefOf(id), dividingSource(skip, testCase.numerator))
		channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 5, EvaluateIntervalSeconds: 1}
		envs = append(envs, testEnvironment(id, channel))
	}
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(envs...), newFakeStates(), publisher)

	//past the first heartbeat and well before the second: everything published
	//beyond what is expected here came from the gate, not from the heartbeat
	time.Sleep(6500 * time.Millisecond)

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			values := publishedValues(t, publisher, idOf(name))
			want := 1
			if testCase.finiteFirst {
				want = 2
			}
			if len(values) != want {
				t.Fatalf("expected %d publishes in six evaluations of a %ds heartbeat, got %d: %v",
					want, heartbeat, len(values), values)
			}
			if testCase.finiteFirst && values[0] != 100 {
				t.Errorf("expected the finite value to be published first, got %v", values[0])
			}
			//and the value that is not finite is not dropped either: the
			//heartbeat still carries it out, which is what keeps the channel
			//from looking dead while its source is broken
			if last := values[len(values)-1]; finite(last) {
				t.Errorf("expected the heartbeat to carry the value that is not finite, got %v", last)
			}
		})
	}
}

// TestTheHeartbeatSendsTheValueOfTheEvaluationItCoincidesWith: an interval that
// is a whole multiple of the evaluation cadence is the shape every document has,
// and it makes the evaluation and the heartbeat due in the same instant on every
// heartbeat. A select picks at random between two ready cases, so without the
// evaluation being served first the same document would publish the current or
// the previous reading on a coin toss.
//
// The assertion is the value, not the timing: the source counts its evaluations,
// so the third publish carrying 30 says three evaluations had run when the
// heartbeat sent. A heartbeat that had won the race would carry 20.
func TestTheHeartbeatSendsTheValueOfTheEvaluationItCoincidesWith(t *testing.T) {
	const id = "env-cov-order"
	//a threshold nothing in this test reaches, so every publish after the first
	//one is a heartbeat; two evaluations per heartbeat
	env := testEnvironment(id, changeChannel(id, 2, everyEvaluation(1e9, 0), 10, 20, 30, 40, 50, 60, 70, 80))
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(12*time.Second, func() bool { return publisher.count() >= 3 }) {
		t.Fatalf("expected three publishes, got %d: %v", publisher.count(), publishedValues(t, publisher, id))
	}
	//10 is the first evaluation, which always publishes; 30 and 50 are the
	//heartbeats at the third and fifth evaluation
	assertValues(t, publishedValues(t, publisher, id)[:3], 10, 30, 50)
}

// TestADueEvaluationIsRunBeforeTheHeartbeatIsServed pins the drain on its own,
// because the live test above cannot: the evaluation ticker is created before
// the heartbeat timer, so its deadline is the earlier one and the runtime
// delivers it first whatever the heartbeat branch does. The case the drain is
// there for is the other one - the runner was busy when both fell due, a script
// that spent the heartbeat instant inside an http call, and both are waiting in
// their channels by the time the select runs again. A select then picks between
// them at random, and taking the heartbeat would send the previous reading.
//
// That state is built here directly rather than provoked with a slow script,
// which would be a timing assumption dressed up as a test.
func TestADueEvaluationIsRunBeforeTheHeartbeatIsServed(t *testing.T) {
	const id = "env-cov-drain"
	//a profile rather than a script: one evaluation is one value, with no engine
	//in between
	channel := profileChannel("ch-1", serviceRefOf(id), 2, flatProfile(230, 0))
	channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 5, EvaluateIntervalSeconds: 1}
	def := testEnvironment(id, channel)

	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, publisher)
	gen := newGeneration(def, nil)
	env := &environment{id: id, gen: gen, state: repo.RuntimeState{EnvironmentId: id}}
	if len(gen.sensors) != 1 || gen.sensors[0].cov == nil {
		t.Fatalf("expected one channel with a resolved trigger, got %d", len(gen.sensors))
	}
	binding := gen.sensors[0]

	//a ticker that has already fired is exactly the state the runner is left in
	//when an evaluation fell due while it was busy
	waiting := time.NewTicker(time.Millisecond)
	defer waiting.Stop()
	time.Sleep(20 * time.Millisecond)

	pending := &latest{}
	if !rt.dueEvaluation(waiting, env, gen, binding, &faultRun{}, pending, &covLogGate{}, time.Now()) {
		t.Fatal("the waiting evaluation was not run, so a heartbeat in this state would send the reading before it")
	}
	if value, known := pending.get(); !known || value != 230.0 {
		t.Errorf("expected the evaluation to leave 230 for the heartbeat, got %v (known=%v)", value, known)
	}
	if publisher.count() != 1 {
		t.Errorf("expected the drained evaluation to publish once, got %d", publisher.count())
	}

	//and with nothing due it must not evaluate at all, or a heartbeat falling
	//between two evaluations would compute an extra value off the grid
	idle := time.NewTicker(time.Hour)
	defer idle.Stop()
	if rt.dueEvaluation(idle, env, gen, binding, &faultRun{}, &latest{}, &covLogGate{}, time.Now()) {
		t.Error("an evaluation was run although none was due")
	}
	if publisher.count() != 1 {
		t.Errorf("expected no further publish, got %d", publisher.count())
	}
}

// recordingWriter is where the throttle test reads the log from. The runner
// goroutine writes it and the test reads it, so it carries its own mutex.
type recordingWriter struct {
	mux  sync.Mutex
	text strings.Builder
}

func (this *recordingWriter) Write(p []byte) (int, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.text.Write(p)
}

func (this *recordingWriter) count(substring string) int {
	this.mux.Lock()
	defer this.mux.Unlock()
	return strings.Count(this.text.String(), substring)
}

// TestARefusedPublishIsReportedOnceRatherThanOnEveryEvaluation: the retry on the
// evaluation cadence is the wanted behaviour and is untouched here - what must
// not follow from it is a line per attempt. At a ten second evaluation behind an
// hourly heartbeat one broken channel writes 360 ERROR lines an hour, and a site
// of them buries every other line in the service.
func TestARefusedPublishIsReportedOnceRatherThanOnEveryEvaluation(t *testing.T) {
	const id = "env-cov-logspam"
	const message = "unable to send channel data"

	log := &recordingWriter{}
	previous := util.Logger
	util.Logger = slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelInfo}))
	t.Cleanup(func() { util.Logger = previous })

	//a value that moves past the threshold on every evaluation, so every
	//evaluation really is an attempt rather than a suppressed one
	env := testEnvironment(id, changeChannel(id, 30, everyEvaluation(5, 0), 10, 100, 200, 300, 400, 500, 600, 700, 800))
	publisher := &fakePublisher{err: errors.New("the platform refused this reading")}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(10*time.Second, func() bool { return publisher.count() >= 4 }) {
		t.Fatalf("expected the refused value to be retried on the evaluation cadence, got %d attempts", publisher.count())
	}
	if lines := log.count(message); lines != 1 {
		t.Errorf("expected one report for %d refused attempts, got %d lines", publisher.count(), lines)
	}

	//and a channel that recovers is heard again when it breaks a second time,
	//which is what the gate has to give up in exchange for the quiet
	attempts := publisher.count()
	publisher.failWith(nil)
	if !waitFor(10*time.Second, func() bool { return publisher.count() >= attempts+2 }) {
		t.Fatal("the channel stopped publishing after the platform recovered")
	}
	attempts = publisher.count()
	publisher.failWith(errors.New("the platform refused this reading again"))
	if !waitFor(10*time.Second, func() bool { return log.count(message) >= 2 }) {
		t.Errorf("the second failure was never reported, %d lines after %d further attempts",
			log.count(message), publisher.count()-attempts)
	}
	if lines := log.count(message); lines != 2 {
		t.Errorf("expected exactly two reports over two failures, got %d", lines)
	}
}

func TestCovOfRejectsWhatValidationRejects(t *testing.T) {
	usable := func(mutate func(channel *domain.Channel)) domain.Channel {
		channel := scriptChannel("ch-1", domain.Sensor, 600, "service", "moses.service.send(1);")
		channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 0.1, EvaluateIntervalSeconds: 10}
		if mutate != nil {
			mutate(&channel)
		}
		return channel
	}

	if cov, ok, reason := covOf(usable(nil)); !ok || reason != "" || cov.evalSeconds != 10 || cov.absolute != 0.1 {
		t.Fatalf("expected the trigger to resolve, got %#v ok=%v reason=%q", cov, ok, reason)
	}
	//the source's own interval is the cadence when the trigger names none
	fromSource := usable(func(channel *domain.Channel) {
		channel.Source.IntervalSeconds = 5
		channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 1}
	})
	if cov, ok, _ := covOf(fromSource); !ok || cov.evalSeconds != 5 {
		t.Fatalf("expected the source interval as the cadence, got %#v ok=%v", cov, ok)
	}

	for name, mutate := range map[string]func(channel *domain.Channel){
		"no trigger at all":     func(channel *domain.Channel) { channel.PublishOnChange = nil },
		"an actuator":           func(channel *domain.Channel) { channel.Direction = domain.Actuator },
		"no heartbeat":          func(channel *domain.Channel) { channel.IntervalSeconds = 0 },
		"an unusable heartbeat": func(channel *domain.Channel) { channel.IntervalSeconds = maxIntervalSeconds + 1 },
		"no threshold":          func(channel *domain.Channel) { channel.PublishOnChange.Absolute = 0 },
		"a negative threshold":  func(channel *domain.Channel) { channel.PublishOnChange.Absolute = -1 },
		"a threshold of NaN":    func(channel *domain.Channel) { channel.PublishOnChange.Absolute = math.NaN() },
		"an infinite threshold": func(channel *domain.Channel) { channel.PublishOnChange.Absolute = math.Inf(1) },
		"two cadences":          func(channel *domain.Channel) { channel.Source.IntervalSeconds = 5 },
		"no cadence":            func(channel *domain.Channel) { channel.PublishOnChange.EvaluateIntervalSeconds = 0 },
		"an unusable cadence": func(channel *domain.Channel) {
			channel.PublishOnChange.EvaluateIntervalSeconds = maxIntervalSeconds + 1
		},
		"slower than the beat": func(channel *domain.Channel) { channel.PublishOnChange.EvaluateIntervalSeconds = 601 },
	} {
		t.Run(name, func(t *testing.T) {
			cov, ok, reason := covOf(usable(mutate))
			if ok {
				t.Fatalf("expected %v to be refused, got %#v", name, cov)
			}
			//a channel without a trigger is not a broken document and says nothing
			if name == "no trigger at all" {
				if reason != "" {
					t.Fatalf("a channel without a trigger must not be reported, got %q", reason)
				}
				return
			}
			if reason == "" {
				t.Fatalf("expected %v to be reported with a reason", name)
			}
		})
	}
}

// TestExceedsChange is the arithmetic on its own, including the values a live
// test cannot produce on demand.
func TestExceedsChange(t *testing.T) {
	for name, testCase := range map[string]struct {
		cov     covSettings
		last    float64
		current float64
		want    bool
	}{
		"absolute, under":             {covSettings{absolute: 5}, 100, 104, false},
		"absolute, exactly at":        {covSettings{absolute: 5}, 100, 105, false},
		"absolute, over":              {covSettings{absolute: 5}, 100, 105.1, true},
		"absolute, downwards":         {covSettings{absolute: 5}, 100, 90, true},
		"relative, under":             {covSettings{relative: 0.1}, 100, 105, false},
		"relative, over":              {covSettings{relative: 0.1}, 100, 120, true},
		"relative against a zero":     {covSettings{relative: 0.5}, 0, 0.001, true},
		"relative, zero to zero":      {covSettings{relative: 0.5}, 0, 0, false},
		"relative, negative base":     {covSettings{relative: 0.1}, -100, -80, true},
		"either one is enough":        {covSettings{absolute: 100, relative: 0.01}, 100, 106, true},
		"no threshold ever fires":     {covSettings{}, 0, 1e9, false},
		"a NaN value is not a change": {covSettings{absolute: 5}, 100, math.NaN(), false},
		"a NaN base is not a change":  {covSettings{absolute: 5}, math.NaN(), 100, false},
		//the infinities are the case NaN does not cover: they compare, and
		//|±Inf - last| is over every finite threshold, so without the guard a
		//channel dividing by zero would publish on every single evaluation
		"an infinite value is not a change":          {covSettings{absolute: 5}, 100, math.Inf(1), false},
		"a negative infinite value is not a change":  {covSettings{absolute: 5}, 100, math.Inf(-1), false},
		"an infinite value against a relative one":   {covSettings{relative: 0.1}, 100, math.Inf(1), false},
		"an infinite value against a zero base":      {covSettings{relative: 0.5}, 0, math.Inf(1), false},
		"an infinite base is not a change":           {covSettings{absolute: 5}, math.Inf(1), 100, false},
		"a negative infinite base is not a change":   {covSettings{absolute: 5}, math.Inf(-1), 100, false},
		"infinity does not differ from itself":       {covSettings{absolute: 5}, math.Inf(1), math.Inf(1), false},
		"the two infinities do not differ either":    {covSettings{absolute: 5}, math.Inf(-1), math.Inf(1), false},
		"a finite move past an infinite base is not": {covSettings{absolute: 5}, math.Inf(1), math.Inf(1) - 1, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := exceedsChange(testCase.cov, testCase.last, testCase.current); got != testCase.want {
				t.Errorf("expected %v, got %v", testCase.want, got)
			}
		})
	}
}

// TestCovSends pins the half of the decision exceedsChange does not make: what
// happens with nothing published yet. The first value goes out so that a fresh
// environment is not silent until it happens to move - but that bypass must not
// become the way a value which is not a number gets past the gate, because
// nothing would ever stop it: there is no base, so there is no comparison to
// fail.
func TestCovSends(t *testing.T) {
	base := func(value float64) *float64 { return &value }
	for name, testCase := range map[string]struct {
		cov     covSettings
		base    *float64
		current float64
		want    bool
	}{
		"the first finite value goes out":      {covSettings{absolute: 5}, nil, 7, true},
		"the first zero goes out too":          {covSettings{absolute: 5}, nil, 0, true},
		"a first NaN waits for the heartbeat":  {covSettings{absolute: 5}, nil, math.NaN(), false},
		"a first infinity waits as well":       {covSettings{absolute: 5}, nil, math.Inf(1), false},
		"so does a first negative infinity":    {covSettings{absolute: 5}, nil, math.Inf(-1), false},
		"with a base it is the threshold":      {covSettings{absolute: 5}, base(100), 106, true},
		"and the threshold can say no":         {covSettings{absolute: 5}, base(100), 104, false},
		"an infinity against a base says no":   {covSettings{absolute: 5}, base(100), math.Inf(1), false},
		"a NaN against a base says no":         {covSettings{absolute: 5}, base(100), math.NaN(), false},
		"a finite value against an infinity":   {covSettings{absolute: 5}, base(math.Inf(1)), 100, false},
		"a finite move after an infinite base": {covSettings{absolute: 5}, base(math.NaN()), 100, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := covSends(testCase.cov, testCase.base, testCase.current); got != testCase.want {
				t.Errorf("expected %v, got %v", testCase.want, got)
			}
		})
	}
}
