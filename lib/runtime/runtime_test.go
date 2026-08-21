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
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// The shortest interval the model allows is one second, so these tests take
// seconds rather than milliseconds. They still run without docker and are part
// of the short mode suite: the alternative, a fake clock, would test the fake.

func TestATickPublishesWithTheExternalRefsOfTheDefinition(t *testing.T) {
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), `moses.service.send("tick");`))
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("no event was published within 4s")
	}
	event := publisher.all()[0]
	if event.deviceRef != deviceRefOf("env-a") {
		t.Errorf("expected the asset's external ref %q, got %q", deviceRefOf("env-a"), event.deviceRef)
	}
	if event.serviceRef != serviceRefOf("env-a") {
		t.Errorf("expected the channel's external ref %q, got %q", serviceRefOf("env-a"), event.serviceRef)
	}
	if event.value != "tick" {
		t.Errorf("expected the value the script sent, got %#v", event.value)
	}
}

// TestStateRoundTripAcrossTheThreeScopes pins the compatibility promise: a
// migrated script uses moses.world, moses.room and moses.device, and get on a
// key that does not exist yet has to return 0 rather than undefined, because
// "get() + 1" is what every legacy change routine does.
func TestStateRoundTripAcrossTheThreeScopes(t *testing.T) {
	code := `
		moses.world.state.set("w", moses.world.state.get("w") + 1);
		moses.room.state.set("r", moses.room.state.get("r") + 2);
		moses.device.state.set("d", moses.device.state.get("d") + 3);
		moses.service.send("" + moses.world.state.get("w") + "," + moses.room.state.get("r") + "," + moses.device.state.get("d"));
	`
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), code))
	publisher := &fakePublisher{}
	states := newFakeStates()
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), states, publisher)

	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("no event was published within 4s")
	}
	if published := publisher.all()[0].value; published != "1,2,3" {
		t.Errorf("expected the seeded zero plus the increments (\"1,2,3\"), got %#v", published)
	}

	//the final flush of Stop is what makes the state observable here
	rt.Stop()
	saves := states.savedFor("env-a")
	if len(saves) == 0 {
		t.Fatal("expected the state to be written")
	}
	saved := saves[len(saves)-1].state
	if got := numberOf(t, saved.Context["w"]); got != 1 {
		t.Errorf("environment context: expected w=1, got %v", got)
	}
	if got := numberOf(t, saved.Zones[testZoneId]["r"]); got != 2 {
		t.Errorf("zone state: expected r=2, got %v", got)
	}
	if got := numberOf(t, saved.Assets[testAssetId]["d"]); got != 3 {
		t.Errorf("asset state: expected d=3, got %v", got)
	}
}

// TestTheNewNamesAreAliasesOfTheLegacyOnes proves they are the same maps and not
// two copies that drift apart.
func TestTheNewNamesAreAliasesOfTheLegacyOnes(t *testing.T) {
	code := `
		moses.world.state.set("x", 5);
		moses.zone.state.set("y", 6);
		moses.asset.state.set("z", 7);
		moses.channel.send("" + moses.environment.state.get("x") + "," + moses.room.state.get("y") + "," + moses.device.state.get("z"));
	`
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), code))
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("no event was published within 4s")
	}
	if published := publisher.all()[0].value; published != "5,6,7" {
		t.Errorf("expected the aliases to see the same state (\"5,6,7\"), got %#v", published)
	}
}

func TestGetRoomAndGetDeviceResolveAgainstTheDefinition(t *testing.T) {
	code := fmt.Sprintf(`
		moses.world.getRoom("%s").getDevice("%s").state.set("k", 9);
		var missingRoom = moses.world.getRoom("no-such-zone");
		var missingDevice = moses.world.getRoom("%s").getDevice("no-such-asset");
		moses.service.send("" + moses.device.state.get("k") + "," + (typeof missingRoom.state) + "," + (typeof missingDevice.state));
	`, testZoneId, testAssetId, testZoneId)
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), code))
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("no event was published within 4s")
	}
	//9 proves the lookup reached the very asset the channel belongs to, and the
	//two "undefined" prove an unknown id yields an empty object instead of a
	//script error
	if published := publisher.all()[0].value; published != "9,undefined,undefined" {
		t.Errorf("expected \"9,undefined,undefined\", got %#v", published)
	}
}

func TestTheInputIsNilOnATick(t *testing.T) {
	code := `moses.service.send(moses.service.input == null ? "nil" : "value");`
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), code))
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("no event was published within 4s")
	}
	if published := publisher.all()[0].value; published != "nil" {
		t.Errorf("expected the input to be nil on a tick, got %#v", published)
	}
}

func TestACommandRunGetsItsInputAndTheResponderAnswers(t *testing.T) {
	//an actuator: it has no interval, so nothing but the command can run it
	code := `moses.service.send(moses.service.input + "-pong");`
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Actuator, 0, serviceRefOf("env-a"), code))
	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	answers := []interface{}{}
	handled := rt.HandleCommand(deviceRefOf("env-a"), serviceRefOf("env-a"), "ping", func(respMsg interface{}) {
		answers = append(answers, respMsg)
	})
	if !handled {
		t.Fatal("expected the runtime to take responsibility for its own device")
	}
	if len(answers) != 1 || answers[0] != "ping-pong" {
		t.Errorf("expected the responder to be called with \"ping-pong\", got %#v", answers)
	}
	//the responder is the only sink of a command run: nothing goes to the event
	//publisher
	if publisher.count() != 0 {
		t.Errorf("expected no event to be published by a command run, got %#v", publisher.all())
	}
}

func TestAnUnknownExternalRefIsNotHandled(t *testing.T) {
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Actuator, 0, serviceRefOf("env-a"), `moses.service.send(1);`))
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), &fakePublisher{})

	if rt.HandleCommand("urn:infai:ses:device:somebody-elses", serviceRefOf("env-a"), nil, func(interface{}) {}) {
		t.Error("expected false for a device no environment owns, so that the legacy runtime gets its chance")
	}
	//the device is ours, the service is not: still ours, because the legacy
	//runtime must not answer for a migrated device
	if !rt.HandleCommand(deviceRefOf("env-a"), "urn:infai:ses:service:unknown", nil, func(interface{}) {}) {
		t.Error("expected true for an own device with an unknown service")
	}
}

// TestTwoChannelsOfOneEnvironmentNeverRunAtTheSameTime is the guarantee the
// legacy world mutex gave and that a migrated script relies on: it may read a
// state, compute and write it back without anything else touching it in between.
func TestTwoChannelsOfOneEnvironmentNeverRunAtTheSameTime(t *testing.T) {
	//target 3 is never reached while the runs are serialised, so every request
	//stays for its hold time and any overlap would be visible
	gate := newBarrier(t, 3, 300*time.Millisecond)
	code := fmt.Sprintf(`httpGet("%s"); moses.service.send(1);`, gate.url())
	env := testEnvironment("env-a",
		scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), code),
		scriptChannel("ch-2", domain.Sensor, 1, "urn:infai:ses:service:env-a-2", code),
	)
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), &fakePublisher{})

	if !waitFor(6*time.Second, func() bool { arrived, _ := gate.stats(); return arrived >= 2 }) {
		arrived, _ := gate.stats()
		t.Fatalf("expected both channels to have run, only %d did", arrived)
	}
	if _, maxInflight := gate.stats(); maxInflight != 1 {
		t.Errorf("expected the channels of one environment to be serialised, saw %d scripts at once", maxInflight)
	}
}

// TestTwoEnvironmentsRunAtTheSameTime is the other half: the serialisation is
// per environment, not global. It fails by timeout if the runtime ever grows a
// lock that spans environments.
func TestTwoEnvironmentsRunAtTheSameTime(t *testing.T) {
	gate := newBarrier(t, 2, 4*time.Second)
	code := fmt.Sprintf(`httpGet("%s"); moses.service.send(1);`, gate.url())
	envs := newFakeEnvironments(
		testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), code)),
		testEnvironment("env-b", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-b"), code)),
	)
	startRuntime(t, testConfig(time.Hour), envs, newFakeStates(), &fakePublisher{})

	if !waitFor(8*time.Second, func() bool { _, maxInflight := gate.stats(); return maxInflight >= 2 }) {
		arrived, maxInflight := gate.stats()
		t.Fatalf("expected two environments to run at the same time, saw at most %d at once (%d arrivals)", maxInflight, arrived)
	}
}

// TestReloadOnlyTouchesItsOwnEnvironment covers what the legacy runtime got
// wrong: an edit stopped and restarted everything.
func TestReloadOnlyTouchesItsOwnEnvironment(t *testing.T) {
	counting := `moses.world.state.set("n", moses.world.state.get("n") + 1); moses.service.send("v1-" + moses.world.state.get("n"));`
	envs := newFakeEnvironments(
		testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), counting)),
		testEnvironment("env-b", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-b"), `moses.service.send("b");`)),
	)
	states := newFakeStates()
	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(time.Hour), envs, states, publisher)

	if !waitFor(4*time.Second, func() bool {
		return len(publisher.forDevice(deviceRefOf("env-a"))) > 0 && len(publisher.forDevice(deviceRefOf("env-b"))) > 0
	}) {
		t.Fatal("expected both environments to have published within 4s")
	}
	beforeB := len(publisher.forDevice(deviceRefOf("env-b")))

	changed := `moses.world.state.set("n", moses.world.state.get("n") + 1); moses.service.send("v2-" + moses.world.state.get("n"));`
	updated := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), changed))
	if err := envs.Put(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	rt.Reload("env-a")

	if !waitFor(4*time.Second, func() bool {
		for _, value := range publisher.forDevice(deviceRefOf("env-a")) {
			if text, ok := value.(string); ok && len(text) > 3 && text[:3] == "v2-" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("expected the reloaded script to run, published: %#v", publisher.forDevice(deviceRefOf("env-a")))
	}

	//the other environment kept ticking through the reload
	if !waitFor(3*time.Second, func() bool { return len(publisher.forDevice(deviceRefOf("env-b"))) > beforeB }) {
		t.Error("expected the other environment to keep publishing across a reload")
	}

	//the runtime state was NOT re-read: it lives in memory and is newer than the
	//store between two flushes
	if count := states.loadCount("env-a"); count != 1 {
		t.Errorf("expected the state to be loaded once, got %d loads", count)
	}
	//and it continued counting instead of starting over
	values := publisher.forDevice(deviceRefOf("env-a"))
	last := values[len(values)-1].(string)
	if last == "v2-1" {
		t.Error("expected the counter to continue across the reload, it started over")
	}
}

// TestWriteBehindWritesOnceInsteadOfPerStateSet is the reason the flusher
// exists: a channel ticking every second must not produce a database write per
// value.
func TestWriteBehindWritesOnceInsteadOfPerStateSet(t *testing.T) {
	code := `moses.world.state.set("n", moses.world.state.get("n") + 1); moses.service.send(moses.world.state.get("n"));`
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), code))
	publisher := &fakePublisher{}
	states := newFakeStates()
	//200ms: several flush rounds fit into the test, and there are far more state
	//changes than writes anyway
	rt := startRuntime(t, testConfig(200*time.Millisecond), newFakeEnvironments(env), states, publisher)

	//a tick first: a fresh environment with empty initial states is not dirty
	//before a script has written something
	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("expected the channel to have run within 4s")
	}
	if !waitFor(4*time.Second, func() bool { return len(states.savedFor("env-a")) > 0 }) {
		t.Fatal("expected the dirty state to be flushed within 4s")
	}
	saves := len(states.savedFor("env-a"))
	//nothing changed since the last flush, so the next rounds write nothing
	time.Sleep(600 * time.Millisecond)
	rt.Stop()

	//one write per tick at most, and at least one write in total: the point is
	//that the writes follow the flush interval, not the state changes
	afterStop := len(states.savedFor("env-a"))
	if afterStop < saves {
		t.Errorf("expected the writes not to disappear, had %d and now %d", saves, afterStop)
	}
	if afterStop > publisher.count()+2 {
		t.Errorf("expected fewer writes than state changes, got %d writes for %d ticks", afterStop, publisher.count())
	}
}

// TestStopFlushesWhatIsStillDirty pins the shutdown path, including the trap
// that the context of Start is already cancelled by then: a flush using it would
// silently write nothing.
func TestStopFlushesWhatIsStillDirty(t *testing.T) {
	code := `moses.world.state.set("n", 42); moses.service.send("done");`
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), code))
	publisher := &fakePublisher{}
	states := newFakeStates()

	ctx, cancel := context.WithCancel(context.Background())
	//an interval that cannot fire during the test, so only the final flush can
	//explain a write
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), states, publisher)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("no event was published within 4s")
	}
	if len(states.savedFor("env-a")) != 0 {
		t.Fatal("expected no periodic write with an hour long flush interval")
	}

	//the service is shutting down: the context goes first, Stop second
	cancel()
	rt.Stop()

	saves := states.savedFor("env-a")
	if len(saves) != 1 {
		t.Fatalf("expected exactly one final write, got %d", len(saves))
	}
	if saves[0].ctxErr != nil {
		t.Errorf("the final flush must not use the cancelled start context, its context carried %v", saves[0].ctxErr)
	}
	if got := numberOf(t, saves[0].state.Context["n"]); got != 42 {
		t.Errorf("expected the final flush to carry n=42, got %v", got)
	}
}

// TestAFailedSaveKeepsTheStateForTheNextRound: losing simulated values because
// the database blinked is not acceptable.
func TestAFailedSaveKeepsTheStateForTheNextRound(t *testing.T) {
	code := `moses.world.state.set("n", 7); moses.service.send("done");`
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), code))
	publisher := &fakePublisher{}
	states := newFakeStates()
	states.saveErr = errors.New("database is down")
	rt := startRuntime(t, testConfig(200*time.Millisecond), newFakeEnvironments(env), states, publisher)

	if !waitFor(4*time.Second, func() bool { return len(states.savedFor("env-a")) >= 2 }) {
		t.Fatal("expected the failed write to be retried on the next round")
	}
	states.mux.Lock()
	states.saveErr = nil
	states.mux.Unlock()

	if !waitFor(2*time.Second, func() bool {
		states.mux.Lock()
		defer states.mux.Unlock()
		stored, ok := states.stored["env-a"]
		return ok && stored.Context["n"] != nil
	}) {
		t.Error("expected the state to arrive once the database is back")
	}
	rt.Stop()
}

func TestRemoveStopsTheEnvironmentAndCleansUpItsState(t *testing.T) {
	code := `moses.world.state.set("n", 1); moses.service.send("tick");`
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), code))
	envs := newFakeEnvironments(env)
	publisher := &fakePublisher{}
	states := newFakeStates()
	rt := startRuntime(t, testConfig(200*time.Millisecond), envs, states, publisher)

	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("no event was published within 4s")
	}

	//the api deletes the definition and its state, then tells the runtime
	if err := envs.Delete(context.Background(), "env-a"); err != nil {
		t.Fatal(err)
	}
	rt.Remove("env-a")

	published := publisher.count()
	//the state document is deleted a second time, after the flush in flight, so
	//that a write on its way cannot leave an orphan behind
	if deleted := states.deletedIds(); len(deleted) != 1 || deleted[0] != "env-a" {
		t.Errorf("expected the runtime state of the removed environment to be deleted, got %#v", deleted)
	}
	//and nothing runs any more
	time.Sleep(1500 * time.Millisecond)
	if publisher.count() != published {
		t.Errorf("expected no further events after the removal, went from %d to %d", published, publisher.count())
	}
	if rt.HandleCommand(deviceRefOf("env-a"), serviceRefOf("env-a"), nil, func(interface{}) {}) {
		t.Error("expected the removed environment to stop claiming its device")
	}
	//a write before the removal is fine, one after it is not: the delete has to be
	//the last word, or the state document of a deleted environment stays behind
	if last := states.lastOpFor("env-a"); last != "delete" {
		t.Errorf("expected the delete to be the last write on the removed state, it was %q", last)
	}
	//and the removal must not have started a flusher of its own that writes again
	time.Sleep(500 * time.Millisecond)
	if last := states.lastOpFor("env-a"); last != "delete" {
		t.Errorf("the state of the removed environment was written again after the delete (%q)", last)
	}
}

// TestRemoveKeepsTheStateWhenTheDefinitionIsStillThere: the second delete is a
// cleanup for a deleted environment, never a way to lose the state of one that
// exists.
func TestRemoveKeepsTheStateWhenTheDefinitionIsStillThere(t *testing.T) {
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), `moses.service.send(1);`))
	envs := newFakeEnvironments(env)
	states := newFakeStates()
	rt := startRuntime(t, testConfig(time.Hour), envs, states, &fakePublisher{})

	rt.Remove("env-a")
	if deleted := states.deletedIds(); len(deleted) != 0 {
		t.Errorf("expected the state of an existing definition to be kept, got deletes %#v", deleted)
	}
}

// TestAnEnvironmentWhoseStateCannotBeLoadedIsNotStarted: seeding from the
// definition and then flushing would overwrite a state that is only temporarily
// unreadable.
func TestAnEnvironmentWhoseStateCannotBeLoadedIsNotStarted(t *testing.T) {
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), `moses.service.send(1);`))
	states := newFakeStates()
	states.loadErr = errors.New("database is down")
	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(200*time.Millisecond), newFakeEnvironments(env), states, publisher)

	time.Sleep(1500 * time.Millisecond)
	if publisher.count() != 0 {
		t.Errorf("expected nothing to run, got %#v", publisher.all())
	}
	if saves := states.savedFor("env-a"); len(saves) != 0 {
		t.Errorf("expected no state to be written over the unreadable one, got %#v", saves)
	}
	if rt.HandleCommand(deviceRefOf("env-a"), serviceRefOf("env-a"), nil, func(interface{}) {}) {
		t.Error("expected an environment that did not start not to claim its device")
	}
}

// TestASourceKindThatIsNotExecutedYetDoesNothing: the document format carries
// profile, dataset and formula sources from the start. Validation rejects them,
// but a document written directly into the database must not make the runtime
// invent values or crash.
func TestASourceKindThatIsNotExecutedYetDoesNothing(t *testing.T) {
	env := testEnvironment("env-a", domain.Channel{
		Id:              "ch-1",
		Name:            "profile",
		Direction:       domain.Sensor,
		ExternalRef:     serviceRefOf("env-a"),
		IntervalSeconds: 1,
		Source:          domain.Source{Kind: domain.SourceProfile, Profile: &domain.ProfileSource{Base: 1}},
	})
	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	time.Sleep(1500 * time.Millisecond)
	if publisher.count() != 0 {
		t.Errorf("expected a source kind that is not implemented to produce nothing, got %#v", publisher.all())
	}
	//the asset is still ours, so the legacy runtime does not get to answer for it
	if !rt.HandleCommand(deviceRefOf("env-a"), serviceRefOf("env-a"), nil, func(interface{}) {}) {
		t.Error("expected the runtime to still own the device of a channel it cannot execute")
	}
}

// TestAnAbsurdIntervalDoesNotStartATicker: seconds times time.Second overflows
// int64 above roughly 292 years, and time.NewTicker panics on the negative
// duration that comes out of it. Validation only rejects a negative interval.
func TestAnAbsurdIntervalDoesNotStartATicker(t *testing.T) {
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1<<62, serviceRefOf("env-a"), `moses.service.send(1);`))
	publisher := &fakePublisher{}
	//a panic in the ticker goroutine would take the test binary down, which is
	//exactly what it would do to the service
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)
	time.Sleep(200 * time.Millisecond)
	if publisher.count() != 0 {
		t.Errorf("expected no events, got %#v", publisher.all())
	}
}

// TestSeedingDoesNotMutateTheDefinition: the definition is what gets exported,
// and a script writing into a nested initial state must not reach it.
func TestSeedingDoesNotMutateTheDefinition(t *testing.T) {
	code := `
		var nested = moses.world.state.get("nested");
		nested.inner = "changed";
		moses.world.state.set("nested", nested);
		moses.service.send("done");
	`
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), code))
	env.Context = map[string]interface{}{"nested": map[string]interface{}{"inner": "original"}}
	publisher := &fakePublisher{}
	states := newFakeStates()
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), states, publisher)

	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("no event was published within 4s")
	}
	rt.Stop()

	nested, ok := env.Context["nested"].(map[string]interface{})
	if !ok {
		t.Fatalf("the definition's context lost its shape: %#v", env.Context)
	}
	if nested["inner"] != "original" {
		t.Errorf("the definition was mutated by the running script: %#v", nested)
	}
}

// TestAnInitialStateDoesNotOverwriteALiveValue: an initial state is a starting
// point, not a default that is reapplied on every restart.
func TestAnInitialStateDoesNotOverwriteALiveValue(t *testing.T) {
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), `moses.service.send(moses.world.state.get("level"));`))
	env.Context = map[string]interface{}{"level": 1}
	states := newFakeStates()
	states.stored["env-a"] = stateWith(map[string]interface{}{"level": 99})
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), states, publisher)

	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("no event was published within 4s")
	}
	if got := numberOf(t, publisher.all()[0].value); got != 99 {
		t.Errorf("expected the stored live value 99 to win over the initial state, got %v", got)
	}
}

func TestStartingTwiceIsRefused(t *testing.T) {
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), &fakePublisher{})
	if err := rt.Start(context.Background()); err == nil {
		t.Error("expected the second Start to be refused")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), &fakePublisher{})
	rt.Stop()
	rt.Stop()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// numberOf accepts every numeric type a value can have after a trip through the
// javascript vm and a store, because which one it is is not what these tests are
// about.
func numberOf(t *testing.T, value interface{}) float64 {
	t.Helper()
	switch typed := value.(type) {
	case int:
		return float64(typed)
	case int32:
		return float64(typed)
	case int64:
		return float64(typed)
	case float32:
		return float64(typed)
	case float64:
		return typed
	default:
		t.Fatalf("expected a number, got %#v of type %T", value, value)
		return 0
	}
}

func stateWith(context map[string]interface{}) repo.RuntimeState {
	return repo.RuntimeState{
		EnvironmentId: "env-a",
		Context:       context,
		Zones:         map[string]map[string]interface{}{},
		Assets:        map[string]map[string]interface{}{},
	}
}

// ---------------------------------------------------------------------------
// device state reporting
// ---------------------------------------------------------------------------

// fakeStateLogger records which devices were reported as online.
type fakeStateLogger struct {
	mux  sync.Mutex
	refs []string
	err  error
}

func (this *fakeStateLogger) LogDeviceConnect(id string) error {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.refs = append(this.refs, id)
	return this.err
}

func (this *fakeStateLogger) seen() []string {
	this.mux.Lock()
	defer this.mux.Unlock()
	result := append([]string{}, this.refs...)
	sort.Strings(result)
	return result
}

// startWithStateLogger is startRuntime plus an injected device state logger.
func startWithStateLogger(t *testing.T, logger deviceStateLogger, envs *fakeEnvironments, publisher *fakePublisher) *Runtime {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rt := newRuntime(testConfig(time.Hour), envs, newFakeStates(), publisher)
	rt.stateLogger = logger
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("unable to start the runtime: %v", err)
	}
	t.Cleanup(rt.Stop)
	return rt
}

// the legacy runtime reports every device of a started world as online. Without
// the same report a migrated device shows as offline in the platform after the
// cutover, which is what the rest of the platform reads to decide it is alive.
func TestStartReportsEveryDeviceOfAnEnvironmentAsOnline(t *testing.T) {
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), "moses.service.send(1);"))
	//an asset without any executable channel is still a platform device and
	//must be reported, exactly like the legacy runtime reports every device
	env.Zones[0].Assets = append(env.Zones[0].Assets, domain.Asset{
		Id:             "asset-2",
		Name:           "meter",
		Kind:           domain.AssetMeter,
		ExternalRef:    "urn:infai:ses:device:second",
		ExternalTypeId: "urn:infai:ses:device-type:test",
		InitialStates:  map[string]interface{}{},
	})

	logger := &fakeStateLogger{}
	startWithStateLogger(t, logger, newFakeEnvironments(env), &fakePublisher{})

	expected := []string{deviceRefOf("env-a"), "urn:infai:ses:device:second"}
	sort.Strings(expected)
	if got := logger.seen(); !reflect.DeepEqual(got, expected) {
		t.Fatalf("expected every device reported online, got %v want %v", got, expected)
	}
}

// the connection log is a side channel: if it fails the simulation still has to
// run, only the displayed state is stale until the next start.
func TestAFailingDeviceStateLogDoesNotStopTheEnvironment(t *testing.T) {
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), "moses.service.send(1);"))
	publisher := &fakePublisher{}
	startWithStateLogger(t, &fakeStateLogger{err: errors.New("connection log unreachable")}, newFakeEnvironments(env), publisher)

	if !waitFor(5*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("expected the sensor to keep publishing despite the failing device state log")
	}
}

// a runtime without a state logger must not panic: the api-only wiring in
// lib.New passes one, but the tests and any future embedding may not.
func TestStartWithoutAStateLoggerWorks(t *testing.T) {
	env := testEnvironment("env-a", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-a"), "moses.service.send(1);"))
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)
	if !waitFor(5*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("expected the sensor to publish without a state logger")
	}
}
