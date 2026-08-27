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
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// backfillWindow is a day that has definitely passed, so the tests never race
// the wall clock the way "now minus something" would.
var (
	backfillFrom = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	backfillTo   = backfillFrom.Add(24 * time.Hour)
)

// hourlyProfile is a profile with a spread, so that a test which claims two runs
// agree is claiming something: without the spread every hour of the day would
// produce the same number and any two runs would match by accident.
func hourlyProfile() domain.ProfileSource {
	hours := make([]float64, 24)
	for i := range hours {
		hours[i] = 0.5 + float64(i)/24
	}
	return domain.ProfileSource{Base: 230, HourFactors: hours, SpreadPercent: 20}
}

// waitForBackfill waits until the job of one environment has stopped running.
func waitForBackfill(t *testing.T, rt *Runtime, id string) BackfillStatus {
	t.Helper()
	var status BackfillStatus
	done := waitFor(15*time.Second, func() bool {
		var err error
		status, err = rt.BackfillStatusOf(id)
		return err == nil && status.State != BackfillRunning
	})
	if !done {
		t.Fatalf("the backfill of %v did not finish, it is %#v", id, status)
	}
	return status
}

// runOneBackfill starts a runtime on the environment, backfills the window and
// returns what was published.
func runOneBackfill(t *testing.T, env domain.Environment, from time.Time, to time.Time) ([]publishedEvent, BackfillStatus) {
	t.Helper()
	publisher := &fakePublisher{}
	//an hour long flush interval keeps the flusher out of the way
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)
	if _, err := rt.StartBackfill(env.Id, from, to); err != nil {
		t.Fatalf("unable to start the backfill: %v", err)
	}
	status := waitForBackfill(t, rt, env.Id)
	return publisher.backfilled(serviceRefOf(env.Id)), status
}

// TestABackfillIsReproducibleFromTheSeedAndTheWindow is the property the whole
// feature is bought for: a model trained on a backfilled dataset can be
// retrained on the same one. Two runtimes, two jobs, the same document.
func TestABackfillIsReproducibleFromTheSeedAndTheWindow(t *testing.T) {
	env := testEnvironment("env-bf", profileChannel("ch-1", serviceRefOf("env-bf"), 3600, hourlyProfile()))
	env.Seed = 4711

	first, _ := runOneBackfill(t, env, backfillFrom, backfillTo)
	second, _ := runOneBackfill(t, env, backfillFrom, backfillTo)

	if len(first) == 0 {
		t.Fatal("the backfill published nothing at all")
	}
	if len(first) != len(second) {
		t.Fatalf("two runs published %d and %d readings", len(first), len(second))
	}
	for i := range first {
		if !first[i].at.Equal(second[i].at) {
			t.Fatalf("reading %d was stamped %v and %v", i, first[i].at, second[i].at)
		}
		if first[i].value != second[i].value {
			t.Fatalf("reading %d at %v was %v and %v", i, first[i].at, first[i].value, second[i].value)
		}
	}

	//and a different seed has to produce a different series, or the comparison
	//above would hold for any two runs
	other := env
	other.Seed = 4712
	third, _ := runOneBackfill(t, other, backfillFrom, backfillTo)
	same := true
	for i := range first {
		if first[i].value != third[i].value {
			same = false
			break
		}
	}
	if same {
		t.Error("two seeds produced the same series, so the seed is not reaching the profile")
	}
}

// TestABackfillCoversTheWholeWindowOnTheChannelInterval pins the arithmetic of
// the loop: one reading at the start, one per whole interval after it, none
// past the end.
func TestABackfillCoversTheWholeWindowOnTheChannelInterval(t *testing.T) {
	env := testEnvironment("env-bf-grid", profileChannel("ch-1", serviceRefOf("env-bf-grid"), 3600, hourlyProfile()))
	//deliberately not a whole number of intervals: 25h30m on an hourly channel
	to := backfillFrom.Add(25*time.Hour + 30*time.Minute)

	events, status := runOneBackfill(t, env, backfillFrom, to)

	if len(events) != 26 {
		t.Fatalf("expected 26 readings over 25h30m on an hourly channel, got %d", len(events))
	}
	for i, event := range events {
		want := backfillFrom.Add(time.Duration(i) * time.Hour)
		if !event.at.Equal(want) {
			t.Fatalf("reading %d was stamped %v, expected %v", i, event.at, want)
		}
		if event.at.After(to) {
			t.Fatalf("reading %d at %v lies past the end of the window", i, event.at)
		}
	}
	if status.Published != 26 {
		t.Errorf("the status counted %d readings, %d were published", status.Published, len(events))
	}
	if status.State != BackfillDone {
		t.Errorf("expected the job to be done, it is %v", status.State)
	}
}

// TestABackfillReadsTheProfileClockTheWayTheLiveRuntimeDoes is the timezone
// trap. A profile's hour and weekday factors are read off the instant in
// whatever location it carries; the live path hands profileValue a local
// time.Now(), while a window arrives over the api as RFC3339 and is usually
// UTC. Without converting, every backfilled day profile would sit at this
// server's zone offset away from the live one - a silent, plausible-looking
// error in exactly the data a model is trained on.
func TestABackfillReadsTheProfileClockTheWayTheLiveRuntimeDoes(t *testing.T) {
	//a fixed non-UTC zone, so the assertion means the same on every machine
	previous := time.Local
	time.Local = time.FixedZone("BACKFILLTEST", 5*3600)
	t.Cleanup(func() { time.Local = previous })

	//zero everywhere but the fifth hour, which is 00:00 UTC in this zone
	hours := make([]float64, 24)
	hours[5] = 1
	profile := domain.ProfileSource{Base: 230, HourFactors: hours}

	env := testEnvironment("env-bf-tz", profileChannel("ch-1", serviceRefOf("env-bf-tz"), 3600, profile))
	events, _ := runOneBackfill(t, env, backfillFrom, backfillTo)

	if len(events) == 0 {
		t.Fatal("the backfill published nothing at all")
	}
	for _, event := range events {
		value, ok := event.value.(float64)
		if !ok {
			t.Fatalf("expected a number, got %T", event.value)
		}
		//the only non-zero hour is the one that is 05:00 in the local zone
		local := event.at.In(time.Local)
		if local.Hour() == 5 {
			if value != 230 {
				t.Fatalf("expected 230 at %v (local %v), got %v", event.at, local, value)
			}
			continue
		}
		if value != 0 {
			t.Fatalf("expected 0 at %v (local %v), got %v - the profile is being read in the wrong zone", event.at, local, value)
		}
	}
}

// TestABackfillDoesNotMoveTheAnchorOfTheLiveSimulation: a looping dataset
// replays relative to a persisted anchor. The job needs its own - the live one
// lies after the window, so every instant of it would count as "not started" -
// but writing that anchor back would make the live channel of a running
// environment jump weeks in its data.
func TestABackfillDoesNotMoveTheAnchorOfTheLiveSimulation(t *testing.T) {
	csv := "Zeit;strom\n2026-01-05 00:00;1,0\n2026-01-05 01:00;2,0\n2026-01-05 02:00;4,0\n"
	store := &fakeRuntimeDatasets{
		meta:    repo.DatasetMeta{Id: "d1", Owner: "user-a", Name: "Lastgang", Timezone: "Europe/Berlin"},
		content: []byte(csv),
	}
	source := replaySource(domain.ResampleHold, domain.AnchorLoop)
	channel := datasetChannel("env-bf-anchor", source)
	//an hour long tick, so the live channel never runs during the test and the
	//anchor can only be moved by the job
	channel.IntervalSeconds = 3600
	env := testEnvironment("env-bf-anchor", channel)

	const liveAnchor = int64(1_700_000_000)
	states := newFakeStates()
	states.stored["env-bf-anchor"] = repo.RuntimeState{
		EnvironmentId: "env-bf-anchor",
		Context:       map[string]interface{}{},
		Zones:         map[string]map[string]interface{}{},
		Assets:        map[string]map[string]interface{}{},
		Anchors:       map[string]int64{"ch-1": liveAnchor},
	}

	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), states, store, publisher)
	if err := rt.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)
	if _, err := rt.StartBackfill("env-bf-anchor", backfillFrom, backfillTo); err != nil {
		t.Fatalf("unable to start the backfill: %v", err)
	}
	status := waitForBackfill(t, rt, "env-bf-anchor")
	if status.Published == 0 {
		t.Fatalf("the backfill published nothing: %#v", status.Channels)
	}

	rt.mux.RLock()
	live := rt.envs["env-bf-anchor"]
	rt.mux.RUnlock()
	live.mux.Lock()
	anchor, known := live.state.Anchors["ch-1"]
	dirty := live.dirty
	live.mux.Unlock()

	if !known || anchor != liveAnchor {
		t.Errorf("the backfill moved the live replay anchor from %d to %d", liveAnchor, anchor)
	}
	if dirty {
		t.Error("the backfill marked the environment state dirty, so it wrote into it")
	}

	//and it replayed against an anchor of its own, not the persisted one. Both
	//anchors produce values here, so the discriminator has to be the values
	//themselves rather than whether anything was published at all.
	rt.mux.RLock()
	points := live.gen.series["ch-1"]
	rt.mux.RUnlock()
	if len(points) < 2 {
		t.Fatalf("the dataset did not load, %d points", len(points))
	}
	events := publisher.backfilled(serviceRefOf("env-bf-anchor"))
	if len(events) == 0 {
		t.Fatal("the backfill published nothing")
	}
	wanted, playable := replayValue(source, points, backfillFrom.Unix(), backfillFrom.In(time.Local), 3600)
	if !playable {
		t.Fatal("the window anchor produces nothing, so the test cannot tell the two apart")
	}
	if events[0].value != wanted {
		t.Errorf("the first reading was %v, expected %v from an anchor at the window start", events[0].value, wanted)
	}
	withLiveAnchor, _ := replayValue(source, points, liveAnchor, backfillFrom.In(time.Local), 3600)
	if withLiveAnchor == wanted {
		t.Skip("both anchors happen to produce the same first value here, so this cannot discriminate")
	}
}

// TestABackfillDoesNotMoveTheCumulativeCounterOfTheLiveSimulation: a cumulative
// profile publishes a meter reading the live path keeps in the asset state. The
// job runs a counter of its own, from zero, and leaves the live one alone.
func TestABackfillDoesNotMoveTheCumulativeCounterOfTheLiveSimulation(t *testing.T) {
	const base = 3600.0 //an hourly rate of 3600 makes one second worth exactly 1
	const interval = int64(3600)
	profile := domain.ProfileSource{Base: base, Cumulative: true}
	env := testEnvironment("env-bf-cum", profileChannel("ch-1", serviceRefOf("env-bf-cum"), interval, profile))

	const liveCounter = 5000.0
	states := newFakeStates()
	states.stored["env-bf-cum"] = repo.RuntimeState{
		EnvironmentId: "env-bf-cum",
		Context:       map[string]interface{}{},
		Zones:         map[string]map[string]interface{}{},
		Assets:        map[string]map[string]interface{}{testAssetId: {"ch-1": liveCounter}},
	}

	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), states, publisher)
	if _, err := rt.StartBackfill("env-bf-cum", backfillFrom, backfillTo); err != nil {
		t.Fatalf("unable to start the backfill: %v", err)
	}
	waitForBackfill(t, rt, "env-bf-cum")

	events := publisher.backfilled(serviceRefOf("env-bf-cum"))
	if len(events) != 25 {
		t.Fatalf("expected 25 hourly readings over a day, got %d", len(events))
	}
	//the counter is a ramp of its own, starting at the first tick's share
	previous := 0.0
	for i, event := range events {
		value := event.value.(float64)
		want := float64(i+1) * base * float64(interval) / 3600
		if math.Abs(value-want) > 1e-6 {
			t.Fatalf("reading %d was %v, expected the meter to stand at %v", i, value, want)
		}
		if value <= previous {
			t.Fatalf("reading %d did not advance the meter: %v after %v", i, value, previous)
		}
		previous = value
	}

	rt.mux.RLock()
	live := rt.envs["env-bf-cum"]
	rt.mux.RUnlock()
	live.mux.Lock()
	counter := live.state.Assets[testAssetId]["ch-1"]
	live.mux.Unlock()
	if counter != liveCounter {
		t.Errorf("the backfill moved the live meter reading from %v to %v", liveCounter, counter)
	}
}

// TestOnlyOneBackfillPerEnvironmentRunsAtATime: two jobs over overlapping
// windows write two readings per instant, and timescale keeps both.
func TestOnlyOneBackfillPerEnvironmentRunsAtATime(t *testing.T) {
	env := testEnvironment("env-bf-once", profileChannel("ch-1", serviceRefOf("env-bf-once"), 60, hourlyProfile()))
	publisher := &fakePublisher{gate: make(chan struct{})}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if _, err := rt.StartBackfill("env-bf-once", backfillFrom, backfillTo); err != nil {
		t.Fatalf("unable to start the first backfill: %v", err)
	}
	if _, err := rt.StartBackfill("env-bf-once", backfillFrom, backfillTo); !errors.Is(err, ErrBackfillRunning) {
		t.Fatalf("expected ErrBackfillRunning for the second job, got %v", err)
	}

	close(publisher.gate)
	waitForBackfill(t, rt, "env-bf-once")

	//and once it is finished another one is allowed
	if _, err := rt.StartBackfill("env-bf-once", backfillFrom, backfillTo); err != nil {
		t.Errorf("expected a second job after the first finished, got %v", err)
	}
}

// TestDeletingAnEnvironmentEndsItsBackfill: the job publishes to platform
// devices that are being deleted with the environment.
func TestDeletingAnEnvironmentEndsItsBackfill(t *testing.T) {
	env := testEnvironment("env-bf-del", profileChannel("ch-1", serviceRefOf("env-bf-del"), 60, hourlyProfile()))
	publisher := &fakePublisher{gate: make(chan struct{})}
	envs := newFakeEnvironments(env)
	rt := startRuntime(t, testConfig(time.Hour), envs, newFakeStates(), publisher)

	if _, err := rt.StartBackfill("env-bf-del", backfillFrom, backfillTo); err != nil {
		t.Fatalf("unable to start the backfill: %v", err)
	}
	if err := envs.Delete(t.Context(), "env-bf-del"); err != nil {
		t.Fatal(err)
	}
	rt.Remove("env-bf-del")
	close(publisher.gate)

	status := waitForBackfill(t, rt, "env-bf-del")
	if status.State != BackfillCancelled {
		t.Errorf("expected the job to be cancelled, it is %v", status.State)
	}
	//a full day on a minute interval is 1441 readings; a cancelled job must not
	//have got anywhere near that
	if status.Published >= 1441 {
		t.Errorf("the cancelled job still published %d of 1441 readings", status.Published)
	}
}

// TestABackfillSaysWhyItSkippedAChannel. A channel that produced nothing is
// otherwise indistinguishable from one that was never asked to.
func TestABackfillSaysWhyItSkippedAChannel(t *testing.T) {
	profile := hourlyProfile()
	ref := func(suffix string) string { return serviceRefOf("env-bf-skip") + ":" + suffix }

	scripted := scriptChannel("ch-script", domain.Sensor, 3600, ref("script"), "moses.service.send(1);")
	formula := domain.Channel{
		Id: "ch-formula", Name: "formula", Direction: domain.Sensor, ExternalRef: ref("formula"), IntervalSeconds: 3600,
		Source: domain.Source{Kind: domain.SourceFormula, Formula: &domain.FormulaSource{
			Expression: "a", Inputs: map[string]string{"a": "channel:ch-good"}}},
	}
	actuator := profileChannel("ch-actuator", ref("actuator"), 0, profile)
	actuator.Direction = domain.Actuator
	noService := profileChannel("ch-no-service", "", 3600, profile)
	noTimePath := profileChannel("ch-no-time-path", ref("plain"), 3600, profile)
	good := profileChannel("ch-good", ref("good"), 3600, profile)

	env := testEnvironment("env-bf-skip", scripted, formula, actuator, noService, noTimePath, good)
	publisher := &fakePublisher{shapeErr: map[string]error{ref("plain"): devices.ErrNoTimePath}}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	if _, err := rt.StartBackfill("env-bf-skip", backfillFrom, backfillTo); err != nil {
		t.Fatalf("unable to start the backfill: %v", err)
	}
	status := waitForBackfill(t, rt, "env-bf-skip")

	reasons := map[string]string{}
	backfillable := map[string]bool{}
	for _, channel := range status.Channels {
		reasons[channel.ChannelId] = channel.SkipReason
		backfillable[channel.ChannelId] = channel.Backfillable
	}
	if status.ChannelsTotal != 6 || status.ChannelsDone != 6 {
		t.Errorf("expected all six channels to be reported, got %d of %d", status.ChannelsDone, status.ChannelsTotal)
	}
	for id, expected := range map[string]string{
		"ch-script":       "stateful",
		"ch-formula":      "derived from other channels",
		"ch-actuator":     "does not publish on a schedule",
		"ch-no-service":   "no platform service",
		"ch-no-time-path": devices.ErrNoTimePath.Error(),
	} {
		if backfillable[id] {
			t.Errorf("%v was treated as backfillable", id)
			continue
		}
		if !strings.Contains(reasons[id], expected) {
			t.Errorf("expected the reason for %v to mention %q, got %q", id, expected, reasons[id])
		}
	}
	if !backfillable["ch-good"] || reasons["ch-good"] != "" {
		t.Errorf("expected ch-good to be backfilled, got %q", reasons["ch-good"])
	}
	if len(publisher.backfilled(ref("good"))) != 25 {
		t.Errorf("expected the one usable channel to publish 25 readings, got %d", len(publisher.backfilled(ref("good"))))
	}
	for _, skipped := range []string{ref("script"), ref("formula"), ref("actuator"), ref("plain")} {
		if published := len(publisher.backfilled(skipped)); published != 0 {
			t.Errorf("a skipped channel published %d readings to %v", published, skipped)
		}
	}
}

// TestABackfillCountsAndReportsAFailedReading: a job that mostly worked still
// has to say what went wrong, and must not stop at the first refusal.
func TestABackfillCountsAndReportsAFailedReading(t *testing.T) {
	env := testEnvironment("env-bf-fail", profileChannel("ch-1", serviceRefOf("env-bf-fail"), 3600, hourlyProfile()))
	refused := backfillFrom.Add(3 * time.Hour)
	publisher := &fakePublisher{failAt: func(at time.Time) error {
		if at.Equal(refused) {
			return errors.New("the platform refused this reading")
		}
		return nil
	}}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)
	if _, err := rt.StartBackfill("env-bf-fail", backfillFrom, backfillTo); err != nil {
		t.Fatalf("unable to start the backfill: %v", err)
	}
	status := waitForBackfill(t, rt, "env-bf-fail")

	if status.State != BackfillDone {
		t.Errorf("one refused reading must not fail the job, it is %v", status.State)
	}
	if len(status.Channels) != 1 {
		t.Fatalf("expected one channel, got %d", len(status.Channels))
	}
	channel := status.Channels[0]
	if channel.Failed != 1 {
		t.Errorf("expected exactly one failed reading, got %d", channel.Failed)
	}
	if channel.Published != 24 {
		t.Errorf("expected the other 24 readings to go out, got %d", channel.Published)
	}
	if !strings.Contains(channel.LastError, "refused") {
		t.Errorf("expected the refusal to be reported, got %q", channel.LastError)
	}
}

// TestAPanickingBackfillFailsTheJobInsteadOfTheService: the job runs in a
// goroutine of its own, so an unhandled panic in it would take the whole
// service down over one bad environment. It has to end up in the status of the
// caller who is polling for it instead.
func TestAPanickingBackfillFailsTheJobInsteadOfTheService(t *testing.T) {
	env := testEnvironment("env-bf-panic", profileChannel("ch-1", serviceRefOf("env-bf-panic"), 3600, hourlyProfile()))
	publisher := &fakePublisher{failAt: func(at time.Time) error {
		panic("a reconstruction bug")
	}}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)
	if _, err := rt.StartBackfill("env-bf-panic", backfillFrom, backfillTo); err != nil {
		t.Fatalf("unable to start the backfill: %v", err)
	}
	status := waitForBackfill(t, rt, "env-bf-panic")

	if status.State != BackfillFailed {
		t.Errorf("expected the job to be failed, it is %v", status.State)
	}
	if !strings.Contains(status.Error, "a reconstruction bug") {
		t.Errorf("expected the panic to be reported, got %q", status.Error)
	}
	if status.FinishedAt == nil {
		t.Error("a failed job still has to say when it stopped")
	}

	//and the runtime is still usable afterwards
	if _, err := rt.StartBackfill("env-bf-panic", backfillFrom, backfillTo); err != nil {
		t.Errorf("the runtime did not survive the panic: %v", err)
	}
}

// TestStoppingTheRuntimeEndsAnyRunningBackfill: Stop waits for the workers, so
// a job that is still registering itself must not slip past that wait.
func TestStoppingTheRuntimeEndsAnyRunningBackfill(t *testing.T) {
	env := testEnvironment("env-bf-stop", profileChannel("ch-1", serviceRefOf("env-bf-stop"), 60, hourlyProfile()))
	publisher := &fakePublisher{gate: make(chan struct{})}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, publisher)
	if err := rt.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.StartBackfill("env-bf-stop", backfillFrom, backfillTo); err != nil {
		t.Fatalf("unable to start the backfill: %v", err)
	}

	stopped := make(chan struct{})
	go func() {
		rt.Stop()
		close(stopped)
	}()
	close(publisher.gate)
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("Stop did not return while a backfill was running")
	}

	status, err := rt.BackfillStatusOf("env-bf-stop")
	if err != nil {
		t.Fatalf("the status was forgotten by the stop: %v", err)
	}
	if status.State == BackfillRunning {
		t.Errorf("the job is still running after the runtime stopped: %#v", status)
	}
	//and no further job is accepted
	if _, err = rt.StartBackfill("env-bf-stop", backfillFrom, backfillTo); !errors.Is(err, repo.ErrNotRunning) {
		t.Errorf("expected a stopped runtime to refuse a job, got %v", err)
	}
}

func TestTheBackfillOfAnUnknownEnvironmentIsNotRunning(t *testing.T) {
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), &fakePublisher{})
	if _, err := rt.StartBackfill("nobody", backfillFrom, backfillTo); !errors.Is(err, repo.ErrNotRunning) {
		t.Errorf("expected ErrNotRunning, got %v", err)
	}
	if _, err := rt.BackfillStatusOf("nobody"); !errors.Is(err, ErrNoBackfill) {
		t.Errorf("expected ErrNoBackfill, got %v", err)
	}
}

// TestAWindowThatCannotBeServedIsRefusedBeforeTheJobStarts pins the boundaries.
// The clock is passed in rather than read, so none of these depend on when the
// test runs.
func TestAWindowThatCannotBeServedIsRefusedBeforeTheJobStarts(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	for name, testCase := range map[string]struct {
		from     time.Time
		to       time.Time
		refused  bool
		contains string
	}{
		"a plain past window":       {now.Add(-7 * day), now.Add(-day), false, ""},
		"ending exactly now":        {now.Add(-day), now, false, ""},
		"exactly the maximum span":  {now.Add(-MaxBackfillSpan), now, false, ""},
		"one second over the span":  {now.Add(-MaxBackfillSpan - time.Second), now, true, "more than the"},
		"from equals to":            {now, now, true, "before to"},
		"reversed":                  {now.Add(-day), now.Add(-2 * day), true, "before to"},
		"in the future":             {now.Add(day), now.Add(2 * day), true, "future"},
		"ending past the tolerance": {now.Add(-day), now.Add(2 * time.Minute), true, "future"},
		"before the platform":       {time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), now, true, "which is not a window"},
		"zero from":                 {time.Time{}, now, true, "both required"},
		"zero to":                   {now.Add(-day), time.Time{}, true, "both required"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := validateBackfillWindow(testCase.from, testCase.to, now)
			if !testCase.refused {
				if err != nil {
					t.Fatalf("expected the window to be accepted, got %v", err)
				}
				return
			}
			rangeError := &BackfillRangeError{}
			if !errors.As(err, &rangeError) {
				t.Fatalf("expected a BackfillRangeError, got %v", err)
			}
			if !strings.Contains(rangeError.Error(), testCase.contains) {
				t.Errorf("expected the reason to mention %q, got %q", testCase.contains, rangeError.Error())
			}
		})
	}
}

// TestAWindowEndingWithinTheClockSkewIsCutOffAtThePresent: a caller computes
// "now" on its own clock, so a window ending a few hundred milliseconds ahead of
// this one is a skew and not a mistake.
func TestAWindowEndingWithinTheClockSkewIsCutOffAtThePresent(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	from, to, err := validateBackfillWindow(now.Add(-time.Hour), now.Add(500*time.Millisecond), now)
	if err != nil {
		t.Fatalf("expected the window to be accepted, got %v", err)
	}
	if !to.Equal(now) {
		t.Errorf("expected the window to be cut off at %v, it ends at %v", now, to)
	}
	if !from.Equal(now.Add(-time.Hour)) {
		t.Errorf("the start was moved to %v", from)
	}
}

// TestAWindowTooDenseToPublishIsRefused: every reading is published
// synchronously, so the number of them is the runtime of the job. A year of
// one-second data is not a job that would finish.
func TestAWindowTooDenseToPublishIsRefused(t *testing.T) {
	env := testEnvironment("env-bf-dense", profileChannel("ch-1", serviceRefOf("env-bf-dense"), 1, hourlyProfile()))
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), &fakePublisher{})

	from := time.Now().Add(-90 * 24 * time.Hour)
	_, err := rt.StartBackfill("env-bf-dense", from, time.Now())
	rangeError := &BackfillRangeError{}
	if !errors.As(err, &rangeError) {
		t.Fatalf("expected 90 days of one-second data to be refused, got %v", err)
	}
	if !strings.Contains(rangeError.Error(), "readings") {
		t.Errorf("expected the reason to name the reading count, got %q", rangeError.Error())
	}
}

func TestBackfillTicksCountsTheReadingsOfAWindow(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for name, testCase := range map[string]struct {
		interval int64
		to       time.Time
		want     int64
	}{
		"one interval exactly":  {60, from.Add(time.Minute), 2},
		"a partial interval":    {60, from.Add(90 * time.Second), 2},
		"a day of minutes":      {60, from.Add(24 * time.Hour), 1441},
		"no schedule":           {0, from.Add(time.Hour), 0},
		"a negative interval":   {-1, from.Add(time.Hour), 0},
		"an unusable interval":  {maxIntervalSeconds + 1, from.Add(time.Hour), 0},
		"an empty window":       {60, from, 1},
		"sub-interval window":   {3600, from.Add(time.Minute), 1},
		"interval at the limit": {maxIntervalSeconds, from.Add(time.Hour), 1},
	} {
		t.Run(name, func(t *testing.T) {
			if got := backfillTicks(testCase.interval, from, testCase.to); got != testCase.want {
				t.Errorf("expected %d readings, got %d", testCase.want, got)
			}
		})
	}
}

// TestABackfillDoesNotDisturbTheLiveChannel: the live simulation of the same
// environment keeps running while a job does, and its readings stay untimed.
func TestABackfillDoesNotDisturbTheLiveChannel(t *testing.T) {
	env := testEnvironment("env-bf-live", profileChannel("ch-1", serviceRefOf("env-bf-live"), 1, hourlyProfile()))
	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	//a short window, so the job is over quickly while the live ticker runs on
	if _, err := rt.StartBackfill("env-bf-live", backfillFrom, backfillFrom.Add(time.Minute)); err != nil {
		t.Fatalf("unable to start the backfill: %v", err)
	}
	waitForBackfill(t, rt, "env-bf-live")

	if !waitFor(4*time.Second, func() bool {
		for _, event := range publisher.all() {
			if event.live {
				return true
			}
		}
		return false
	}) {
		t.Fatal("the live channel stopped publishing while the backfill ran")
	}
	for _, event := range publisher.all() {
		if event.live {
			continue
		}
		if event.at.Before(backfillFrom) || event.at.After(backfillFrom.Add(time.Minute)) {
			t.Errorf("a backfilled reading was stamped %v, outside the window", event.at)
		}
	}
}

// TestBackfillThroughput measures the loop itself, with the platform faked
// away. It asserts only that the loop is not pathologically slow; the number it
// logs is what the documented volume limit is reasoned from, and the real rate
// in a deployment is set by the synchronous kafka and postgres writes rather
// than by anything here.
func TestBackfillThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput measurement")
	}
	//50 000 readings: a day of one-second data, near what a real job does
	env := testEnvironment("env-bf-rate", profileChannel("ch-1", serviceRefOf("env-bf-rate"), 1, hourlyProfile()))
	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	const readings = 50000
	from := backfillFrom
	to := from.Add(readings * time.Second)

	started := time.Now()
	if _, err := rt.StartBackfill("env-bf-rate", from, to); err != nil {
		t.Fatalf("unable to start the backfill: %v", err)
	}
	status := waitForBackfill(t, rt, "env-bf-rate")
	elapsed := time.Since(started)

	if status.Published < readings {
		t.Fatalf("expected %d readings, got %d", readings, status.Published)
	}
	perReading := elapsed / time.Duration(status.Published)
	t.Logf("%d readings in %v, %v per reading, %.0f per second",
		status.Published, elapsed, perReading, float64(status.Published)/elapsed.Seconds())
	if perReading > 100*time.Microsecond {
		t.Errorf("the loop needs %v per reading with the platform faked away, which is too much to be the loop itself", perReading)
	}
}
