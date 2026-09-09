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
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/util"
)

// followingChannel is a platform-origin dataset channel with follow set, the
// only anchor follow accepts. followEvery bypasses domain.MinFollowEvery,
// like every runtime test builds its document directly rather than through
// Validate.
func followingChannel(envId string, followEvery string) domain.Channel {
	return domain.Channel{
		Id: "ch-follow", Name: "follow", Direction: domain.Sensor,
		ExternalRef: serviceRefOf(envId), IntervalSeconds: 1,
		Source: domain.Source{Kind: domain.SourceDataset, Dataset: &domain.DatasetSource{
			Origin: domain.OriginPlatform, Ref: "urn:device:follow", ServiceRef: "urn:service:follow",
			Column: "value", Window: "1h",
			Resample: domain.ResampleHold, Anchor: domain.AnchorOriginal,
			Follow: true, FollowEvery: followEvery,
		}},
	}
}

// followingSourceChannel is followingChannel with the channel id, the device
// its fetches are recorded under and the window given, so one document can
// carry two following sources a test can tell apart.
func followingSourceChannel(envId string, channelId string, deviceId string, followEvery string, window string) domain.Channel {
	channel := followingChannel(envId, followEvery)
	channel.Id = channelId
	channel.Name = channelId
	channel.Source.Dataset.Ref = deviceId
	channel.Source.Dataset.Window = window
	return channel
}

// bypassTheFollowTickFloor lowers the floor of the shared follow ticker for one
// test. A stored follow_every is at least a minute (domain.MinFollowEvery) and
// the floor exists for documents that bypassed validation, which is exactly
// what these tests build - no test can wait a minute for a tick.
func bypassTheFollowTickFloor(t *testing.T) {
	t.Helper()
	previous := minFollowTick
	minFollowTick = time.Millisecond
	t.Cleanup(func() { minFollowTick = previous })
}

// recordLog sends the service log into a recorder for the duration of one test,
// so that a test can assert what was and what was not reported.
func recordLog(t *testing.T) *recordingWriter {
	t.Helper()
	log := &recordingWriter{}
	previous := util.Logger
	util.Logger = slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelInfo}))
	t.Cleanup(func() { util.Logger = previous })
	return log
}

// seriesOf reads the loaded points of one channel of a running environment,
// the same way backfill_test.go reads live.gen.series - under env.mux, since
// a follow refresh swaps that map's values under it while this runs live.
func seriesOf(t *testing.T, rt *Runtime, envId string, channelId string) []dataset.Point {
	t.Helper()
	rt.mux.RLock()
	env := rt.envs[envId]
	rt.mux.RUnlock()
	if env == nil {
		t.Fatalf("environment %v is not running", envId)
	}
	env.mux.Lock()
	defer env.mux.Unlock()
	return append([]dataset.Point{}, env.gen.series[channelId]...)
}

func TestAFollowRefreshAppendsNewerPointsWithoutDuplicates(t *testing.T) {
	bypassTheFollowTickFloor(t)
	const id = "env-follow-grow"
	fetcher := &fakeFetcher{
		//index 0 is the initial load: three points ending at the load's own
		//"now". Every refresh after that answers the last known instant again
		//(which must be deduplicated) plus one new point at its own "now", so
		//the series stays anchored to the real clock a real wrapper would answer
		//against, rather than to a fixed instant captured before the test ran.
		pointsFunc: func(index int, start time.Time, end time.Time) ([]dataset.Point, error) {
			if index == 0 {
				return []dataset.Point{
					{Unix: end.Unix() - 2, Value: 11},
					{Unix: end.Unix() - 1, Value: 22},
					{Unix: end.Unix(), Value: 33},
				}, nil
			}
			return []dataset.Point{
				{Unix: start.Unix(), Value: 33},
				{Unix: end.Unix(), Value: 99},
			}, nil
		},
	}
	env := testEnvironment(id, followingChannel(id, "150ms"))
	env.Owner = "owner-follow"
	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, newFakeHistoryJobs(), publisher)
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	if !waitFor(3*time.Second, func() bool { return fetcher.callCount() >= 2 }) {
		t.Fatalf("the follow loop never refreshed, got %d fetch calls", fetcher.callCount())
	}
	//the second call's start is the last instant the first call stored: a
	//refresh asks [last point, now), not the source's own window again
	wantStart := time.Unix(fetcher.endAt(0).Unix(), 0)
	if got := fetcher.startAt(1); !got.Equal(wantStart) {
		t.Errorf("the second fetch started at %v, want the first fetch's last instant %v", got, wantStart)
	}

	if !waitFor(3*time.Second, func() bool { return len(seriesOf(t, rt, id, "ch-follow")) > 3 }) {
		t.Fatal("the series never grew past the three initially fetched points")
	}
	points := seriesOf(t, rt, id, "ch-follow")
	initial := []dataset.Point{{Unix: fetcher.endAt(0).Unix() - 2, Value: 11}, {Unix: fetcher.endAt(0).Unix() - 1, Value: 22}, {Unix: fetcher.endAt(0).Unix(), Value: 33}}
	for i, want := range initial {
		if points[i] != want {
			t.Errorf("point %d changed by the refresh: got %+v, want %+v", i, points[i], want)
		}
	}
	for i := 3; i < len(points); i++ {
		if points[i].Unix <= points[i-1].Unix {
			t.Fatalf("point %d is not strictly after point %d, got %+v after %+v: a refresh duplicated or reordered a point",
				i, i-1, points[i], points[i-1])
		}
		if points[i].Value != 99 {
			t.Errorf("point %d carries %v, want 99 - every point a refresh appends here does", i, points[i].Value)
		}
	}

	//and the newest fetched value reaches a publish through the ordinary
	//replay path, without any cache of the pre-refresh series to invalidate
	if !waitFor(5*time.Second, func() bool {
		for _, event := range publisher.all() {
			if v, ok := event.value.(float64); ok && v == 99 {
				return true
			}
		}
		return false
	}) {
		t.Error("no publish ever carried the newest value the refresh appended")
	}
}

func TestAFollowRefreshErrorKeepsTheOldSeriesAndWarns(t *testing.T) {
	bypassTheFollowTickFloor(t)
	const id = "env-follow-err"
	now := time.Now().Unix()
	initial := []dataset.Point{{Unix: now - 2, Value: 11}, {Unix: now - 1, Value: 22}, {Unix: now, Value: 33}}
	fetcher := &fakeFetcher{points: initial, errSeq: []error{nil, errors.New("wrapper unavailable")}}

	log := recordLog(t)

	env := testEnvironment(id, followingChannel(id, "150ms"))
	env.Owner = "owner-follow"
	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, newFakeHistoryJobs(), publisher)
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	if !waitFor(3*time.Second, func() bool { return fetcher.callCount() >= 2 }) {
		t.Fatalf("the follow loop never attempted a second fetch, got %d calls", fetcher.callCount())
	}
	//give the failed refresh a moment to be handled before the series is read
	if !waitFor(2*time.Second, func() bool { return log.count("unable to refresh a following dataset source") > 0 }) {
		t.Fatal("the failed refresh was never reported")
	}

	points := seriesOf(t, rt, id, "ch-follow")
	if len(points) != len(initial) {
		t.Fatalf("a failed refresh must keep the old series unchanged, got %d points, want %d", len(points), len(initial))
	}
	for i, want := range initial {
		if points[i] != want {
			t.Errorf("point %d changed although the refresh failed: got %+v, want %+v", i, points[i], want)
		}
	}
}

func TestStoppingTheEnvironmentStopsTheFollowRefresh(t *testing.T) {
	bypassTheFollowTickFloor(t)
	const id = "env-follow-stop"
	now := time.Now().Unix()
	fetcher := &fakeFetcher{points: []dataset.Point{{Unix: now - 1, Value: 1}, {Unix: now, Value: 2}}}
	env := testEnvironment(id, followingChannel(id, "150ms"))
	env.Owner = "owner-follow"
	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, newFakeHistoryJobs(), publisher)
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}

	if !waitFor(3*time.Second, func() bool { return fetcher.callCount() >= 2 }) {
		t.Fatalf("the follow loop never refreshed, got %d calls", fetcher.callCount())
	}

	//Stop waits for env.runners, the follow loop's goroutine included, so no
	//fetch started after this call can still be missed by the check below
	rt.Stop()
	stopped := fetcher.callCount()
	time.Sleep(500 * time.Millisecond) //far longer than follow_every, so a loop still running would have refreshed again
	if got := fetcher.callCount(); got != stopped {
		t.Errorf("the follow loop kept refreshing after Stop, %d calls before it, %d after", stopped, got)
	}
}

// TestEveryFollowerIsRefreshedOnItsOwnCadence: the timer of an environment
// ticks at the shortest follow_every among its sources, which must not become
// the cadence of all of them - an hourly source next to a minutely one would
// otherwise be re-read sixty times an hour, against a wrapper and an export
// that answer the same rows every time.
func TestEveryFollowerIsRefreshedOnItsOwnCadence(t *testing.T) {
	bypassTheFollowTickFloor(t)
	const id = "env-follow-cadence"
	const fastDevice = "urn:device:fast"
	const slowDevice = "urn:device:slow"
	fetcher := &fakeFetcher{pointsFunc: func(index int, start time.Time, end time.Time) ([]dataset.Point, error) {
		return []dataset.Point{{Unix: end.Unix() - 1, Value: 11}, {Unix: end.Unix(), Value: 22}}, nil
	}}
	fast := followingSourceChannel(id, "ch-fast", fastDevice, "20ms", "1h")
	slow := followingSourceChannel(id, "ch-slow", slowDevice, "1h", "1h")
	env := testEnvironment(id, fast, slow)
	env.Owner = "owner-follow"
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, newFakeHistoryJobs(), &fakePublisher{})
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	//several ticks of the shared timer have to have happened, or the assertion
	//below would hold on a runtime that never refreshed anything at all
	if !waitFor(5*time.Second, func() bool { return fetcher.callCountOf(fastDevice) >= 6 }) {
		t.Fatalf("the 20ms source was fetched %d times, expected the loop to refresh it repeatedly",
			fetcher.callCountOf(fastDevice))
	}
	if got := fetcher.callCountOf(slowDevice); got != 1 {
		t.Errorf("the hourly source was fetched %d times, expected exactly its initial load: it is refreshed on the cadence of the fast one",
			got)
	}
}

// TestABackfillReadsAFrozenSeriesWhileAFollowerGrowsIt: a job reconstructs a
// window from the points of its environment, and a following source appends to
// exactly that map while it runs. The job therefore takes a snapshot at its
// start - both because a reconstruction has to be one series rather than a
// series that changed halfway through, and because reading a map another
// goroutine writes is a data race.
func TestABackfillReadsAFrozenSeriesWhileAFollowerGrowsIt(t *testing.T) {
	bypassTheFollowTickFloor(t)
	const id = "env-follow-backfill"
	//counted per device rather than off the global call index: the two sources
	//are loaded in document order, but a reload would renumber every later call
	followCalls := 0
	fetcher := &fakeFetcher{pointsOfDevice: map[string]func(index int, start time.Time, end time.Time) ([]dataset.Point, error){
		"urn:device:follow": func(index int, start time.Time, end time.Time) ([]dataset.Point, error) {
			followCalls++
			if followCalls == 1 {
				return []dataset.Point{
					{Unix: end.Unix() - 2, Value: 11}, {Unix: end.Unix() - 1, Value: 22}, {Unix: end.Unix(), Value: 33},
				}, nil
			}
			//one point per refresh, and a strictly newer instant on every call
			//rather than the second the clock happens to stand in: the series has
			//to be written on every tick for the jobs below to be reading a map
			//that is being written
			return []dataset.Point{{Unix: end.Unix() + int64(followCalls), Value: 99}}, nil
		},
		//two points around the window the jobs reconstruct: with the single
		//point this used to answer, Fetch refuses the series, the channel is
		//never bound and no job ever reads it out of the map the refresh writes
		"urn:device:still": func(index int, start time.Time, end time.Time) ([]dataset.Point, error) {
			return []dataset.Point{
				{Unix: backfillFrom.Add(-time.Hour).Unix(), Value: 5},
				{Unix: backfillFrom.Add(time.Hour).Unix(), Value: 7},
			}, nil
		},
	}}
	follower := followingSourceChannel(id, "ch-follow", "urn:device:follow", "2ms", "1h")
	//a second dataset channel that does not follow: a job reads its points out
	//of the same map the refresh of the first one writes
	still := followingSourceChannel(id, "ch-still", "urn:device:still", "", "1h")
	still.Source.Dataset.Follow = false
	still.IntervalSeconds = 60
	still.ExternalRef = serviceRefOf(id) + "-still"
	env := testEnvironment(id, follower, still)
	env.Owner = "owner-follow"
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, newFakeHistoryJobs(), &fakePublisher{})
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	//the refresh has to be running before the first job starts, or the jobs
	//below would read a map nobody writes and prove nothing
	if !waitFor(3*time.Second, func() bool { return len(seriesOf(t, rt, id, "ch-follow")) > 3 }) {
		t.Fatalf("the followed series never grew, %d fetch calls so far", fetcher.callCount())
	}
	if points := seriesOf(t, rt, id, "ch-still"); len(points) != 2 {
		t.Fatalf("the still series holds %d points, the jobs below only read it through a bound channel", len(points))
	}
	before := len(seriesOf(t, rt, id, "ch-follow"))

	//a short window per job, so that twenty of them start while the refresh
	//keeps writing: it is the start of a job that reads the map
	to := backfillFrom.Add(2 * time.Minute)
	for i := 0; i < 20; i++ {
		if _, err := rt.StartBackfill(id, backfillFrom, to); err != nil {
			t.Fatalf("job %d did not start: %v", i, err)
		}
		if status := waitForBackfill(t, rt, id); status.State != BackfillDone {
			t.Fatalf("job %d ended as %v: %v", i, status.State, status.Error)
		}
	}

	if after := len(seriesOf(t, rt, id, "ch-follow")); after <= before {
		t.Errorf("the followed series stopped growing during the jobs, %d points before and %d after", before, after)
	}
}

// TestAFollowerWhoseInitialLoadFailedIsLoadedByTheLoop: a wrapper that is down
// while an environment starts leaves the source with no points at all, and a
// refresh that asks for the tail after its last point has no last point to ask
// after. The loop has to fetch the whole window in that case - otherwise one
// unlucky start silences the channel until somebody edits the document.
func TestAFollowerWhoseInitialLoadFailedIsLoadedByTheLoop(t *testing.T) {
	bypassTheFollowTickFloor(t)
	const id = "env-follow-recover"
	fetcher := &fakeFetcher{pointsFunc: func(index int, start time.Time, end time.Time) ([]dataset.Point, error) {
		if index == 0 {
			return nil, errors.New("the wrapper is unavailable")
		}
		return []dataset.Point{
			{Unix: end.Unix() - 2, Value: 11}, {Unix: end.Unix() - 1, Value: 22}, {Unix: end.Unix(), Value: 99},
		}, nil
	}}
	env := testEnvironment(id, followingChannel(id, "20ms"))
	env.Owner = "owner-follow"
	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, newFakeHistoryJobs(), publisher)
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	//the channel of a source that did not load has no binding, so the publish
	//below is only reachable once the loop loaded it and the environment was
	//built again
	if !waitFor(10*time.Second, func() bool { return len(publisher.forDevice(deviceRefOf(id))) > 0 }) {
		t.Fatalf("the channel never published, %d fetch calls and %d stored points",
			fetcher.callCount(), len(seriesOf(t, rt, id, "ch-follow")))
	}
	if points := seriesOf(t, rt, id, "ch-follow"); len(points) < 2 {
		t.Errorf("the recovered source holds %d points, expected the fetched window", len(points))
	}
}

// TestAFollowRefreshDoesNotInvertItsWindow: the newest stored point can lie
// after this clock - a wrongly stamped import, or a platform clock ahead of
// this one - and [last point, now] would then be a window the wrapper cannot
// answer. One cadence back is a window it can, and nothing of it is stored
// twice because only points after the newest one are appended.
func TestAFollowRefreshDoesNotInvertItsWindow(t *testing.T) {
	bypassTheFollowTickFloor(t)
	const id = "env-follow-future"
	fetcher := &fakeFetcher{pointsFunc: func(index int, start time.Time, end time.Time) ([]dataset.Point, error) {
		if index == 0 {
			//the newest point sits an hour in the future
			ahead := end.Add(time.Hour).Unix()
			return []dataset.Point{{Unix: ahead - 2, Value: 11}, {Unix: ahead - 1, Value: 22}, {Unix: ahead, Value: 33}}, nil
		}
		//and the wrapper answers the present, which is older than what is stored
		return []dataset.Point{{Unix: end.Unix(), Value: 99}}, nil
	}}
	env := testEnvironment(id, followingChannel(id, "20ms"))
	env.Owner = "owner-follow"
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, newFakeHistoryJobs(), &fakePublisher{})
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	if !waitFor(3*time.Second, func() bool { return fetcher.callCount() >= 3 }) {
		t.Fatalf("the follow loop never refreshed, got %d fetch calls", fetcher.callCount())
	}
	for index := 1; index <= 2; index++ {
		start, end := fetcher.startAt(index), fetcher.endAt(index)
		if !start.Before(end) {
			t.Errorf("fetch %d asked for the inverted window [%v, %v]", index, start, end)
		}
		//and it asked for the cadence, not for the hour that lies in between
		if window := end.Sub(start); window > 5*time.Second {
			t.Errorf("fetch %d asked for a window of %v, expected about the follow_every", index, window)
		}
	}
	if points := seriesOf(t, rt, id, "ch-follow"); len(points) != 3 {
		t.Errorf("the series holds %d points, expected the three fetched ones: nothing older than the newest may be appended, got %+v",
			len(points), points)
	}
}

// TestACancelledRefreshIsNotReported: a stop, a reload and a history run all
// cancel the environment context, and a fetch in flight ends with it. That is
// the ordinary way an environment goes down, so it must not leave a warning
// per following source behind in the log of a shutdown.
func TestACancelledRefreshIsNotReported(t *testing.T) {
	bypassTheFollowTickFloor(t)
	const id = "env-follow-cancel"
	now := time.Now().Unix()
	fetcher := &fakeFetcher{
		points: []dataset.Point{{Unix: now - 1, Value: 11}, {Unix: now, Value: 22}},
		//the initial load answers at once, every refresh after it hangs until
		//the environment is cancelled
		fetchGate: make(chan struct{}),
		gateFrom:  1,
	}
	log := recordLog(t)
	env := testEnvironment(id, followingChannel(id, "20ms"))
	env.Owner = "owner-follow"
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, newFakeHistoryJobs(), &fakePublisher{})
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	//Stop is idempotent, and a test that fails before the explicit one below
	//must not leave a runtime running past its own cleanup
	t.Cleanup(rt.Stop)

	if !waitFor(3*time.Second, func() bool { return fetcher.callCount() >= 2 }) {
		t.Fatalf("the follow loop never started a refresh, got %d fetch calls", fetcher.callCount())
	}
	//Stop cancels the environment context and waits for the follow loop, so the
	//refresh above is handled before this returns
	rt.Stop()
	if got := log.count("unable to refresh a following dataset source"); got != 0 {
		t.Errorf("a refresh the shutdown cancelled was reported %d times, expected silence", got)
	}
}

// TestAFollowingSeriesIsTrimmedToItsWindow: a following source appends for as
// long as the environment runs, and window is what says how much of it is
// worth keeping - without the trim a source followed for weeks holds every
// point it ever answered.
func TestAFollowingSeriesIsTrimmedToItsWindow(t *testing.T) {
	bypassTheFollowTickFloor(t)
	const id = "env-follow-trim"
	fetcher := &fakeFetcher{pointsFunc: func(index int, start time.Time, end time.Time) ([]dataset.Point, error) {
		if index == 0 {
			//two points far outside the ten second window, and one at the
			//instant of the load
			return []dataset.Point{
				{Unix: end.Unix() - 3600, Value: 11},
				{Unix: end.Unix() - 1800, Value: 22},
				{Unix: end.Unix(), Value: 33},
			}, nil
		}
		return []dataset.Point{{Unix: end.Unix(), Value: 99}}, nil
	}}
	channel := followingSourceChannel(id, "ch-follow", "urn:device:follow", "20ms", "10s")
	env := testEnvironment(id, channel)
	env.Owner = "owner-follow"
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, newFakeHistoryJobs(), &fakePublisher{})
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	loadedAt := fetcher.endAt(0).Unix()
	//the first refresh that appends a point is the one that trims, and an
	//append only happens once a second has passed
	if !waitFor(5*time.Second, func() bool {
		points := seriesOf(t, rt, id, "ch-follow")
		return len(points) > 0 && points[0].Unix != loadedAt-3600
	}) {
		t.Fatalf("the two points from before the window were never dropped, got %+v", seriesOf(t, rt, id, "ch-follow"))
	}
	cutoff := time.Now().Add(-10 * time.Second).Unix()
	points := seriesOf(t, rt, id, "ch-follow")
	if len(points) < 2 {
		t.Fatalf("the trim left %d points, it may never leave fewer than two", len(points))
	}
	for i, point := range points {
		if point.Unix < cutoff {
			t.Errorf("point %d sits at %d, before the window's start %d: %+v", i, point.Unix, cutoff, points)
		}
	}
}

// TestTrimToWindowKeepsTheLastTwoPoints pins the two boundaries the trim is
// written for: a point exactly at the cutoff still belongs to the window, and a
// series whose newest point is older than its own window keeps two points, or
// the channel would lose its binding and replayValue its span.
func TestTrimToWindowKeepsTheLastTwoPoints(t *testing.T) {
	cutoff := time.Unix(1_000_000, 0)
	for _, tc := range []struct {
		name   string
		points []int64
		want   []int64
	}{
		{"nothing before the cutoff", []int64{1_000_000, 1_000_060}, []int64{1_000_000, 1_000_060}},
		{"a point exactly at the cutoff stays", []int64{999_940, 1_000_000, 1_000_060}, []int64{1_000_000, 1_000_060}},
		{"one second before the cutoff goes", []int64{999_999, 1_000_000, 1_000_060}, []int64{1_000_000, 1_000_060}},
		{"everything before the cutoff leaves two", []int64{1, 2, 3, 4}, []int64{3, 4}},
		{"two points are never trimmed", []int64{1, 2}, []int64{1, 2}},
		{"one point is never trimmed", []int64{1}, []int64{1}},
	} {
		points := make([]dataset.Point, 0, len(tc.points))
		for _, unix := range tc.points {
			points = append(points, dataset.Point{Unix: unix, Value: float64(unix)})
		}
		got := trimToWindow(points, cutoff)
		if len(got) != len(tc.want) {
			t.Errorf("%s: kept %d points, want %d: %+v", tc.name, len(got), len(tc.want), got)
			continue
		}
		for i, unix := range tc.want {
			if got[i].Unix != unix {
				t.Errorf("%s: point %d is %d, want %d", tc.name, i, got[i].Unix, unix)
			}
		}
	}
}

// TestAppendNewerTakesEverythingIntoAnEmptySeries: the merge is also what a
// source that has just been loaded by the loop runs through, so it may not read
// the last instant of a series that has none.
func TestAppendNewerTakesEverythingIntoAnEmptySeries(t *testing.T) {
	fresh := []dataset.Point{{Unix: 10, Value: 1}, {Unix: 10, Value: 2}, {Unix: 20, Value: 3}}
	for _, stored := range [][]dataset.Point{nil, {}} {
		got := appendNewer(stored, fresh)
		//the duplicate instant is dropped here as it is anywhere else
		if len(got) != 2 || got[0].Unix != 10 || got[1].Unix != 20 {
			t.Errorf("an empty stored series has to take the whole fetch, got %+v", got)
		}
	}
}

// TestOnlyAPollableOriginFollows: follow is refused on an uploaded dataset by
// validation, and the loop has to refuse it too - both fetch paths address the
// timescale-wrapper, so a file source would be sent as a device id.
func TestOnlyAPollableOriginFollows(t *testing.T) {
	const id = "env-follow-origins"
	file := followingChannel(id, "20ms")
	file.Id = "ch-file"
	file.Source.Dataset.Origin = domain.OriginFile
	kept := followingChannel(id, "20ms")
	followers := followersOf(testEnvironment(id, file, kept))
	if len(followers) != 1 {
		t.Fatalf("expected only the platform source to follow, got %d followers", len(followers))
	}
	if followers[0].seriesId != kept.Id {
		t.Errorf("the follower is %q, want the platform channel %q", followers[0].seriesId, kept.Id)
	}
}

// TestAFollowingReplayHoldsItsNewestPoint: an original-anchored replay is
// silent for every instant after its last point, which for a following source
// is nearly every tick - its newest measurement is minutes old, and the next
// refresh is a follow_every away. A followed present therefore holds the
// newest measurement instead of falling silent between two refreshes.
func TestAFollowingReplayHoldsItsNewestPoint(t *testing.T) {
	points := []dataset.Point{{Unix: 1000, Value: 100}, {Unix: 1800, Value: 400}}
	source := replaySource(domain.ResampleHold, domain.AnchorOriginal)
	source.Follow = true

	//half an hour after the newest point, which is what a source that
	//publishes hourly looks like on the tick after a refresh
	held, playable := replayValue(source, points, 0, time.Unix(3600, 0), 30)
	if !playable {
		t.Fatal("a following source has to keep playing after its newest point")
	}
	if held != 400 {
		t.Errorf("the held value is %v, want the newest measurement 400", held)
	}

	//before the first point there is still nothing to say, following or not
	if _, playable = replayValue(source, points, 0, time.Unix(500, 0), 30); playable {
		t.Error("before its first point a replay has nothing to hold")
	}

	//and a source that does not follow stays silent, as it did
	still := replaySource(domain.ResampleHold, domain.AnchorOriginal)
	if _, playable = replayValue(still, points, 0, time.Unix(3600, 0), 30); playable {
		t.Error("a source that does not follow must not hold past its last point")
	}
}

// followTestClock drives a follow loop from the test goroutine instead of the
// wall clock: the loop reads its instants here and blocks on the tick channel,
// so a cadence assertion is an exact count rather than a tolerance.
type followTestClock struct {
	mux   sync.Mutex
	now   time.Time
	ticks chan time.Time

	// read carries one token per call the loop makes to now(). Without it the
	// next tick would move the clock forward while the loop is still reading
	// the instant of this one, which skips a due time and makes the count this
	// clock exists to make exact wrong again.
	read chan struct{}
}

func newFollowTestClock(start time.Time) *followTestClock {
	return &followTestClock{now: start, ticks: make(chan time.Time), read: make(chan struct{}, 1)}
}

func (this *followTestClock) hook() *followClock {
	return &followClock{
		now: func() time.Time {
			this.mux.Lock()
			at := this.now
			this.mux.Unlock()
			select {
			case this.read <- struct{}{}:
			default:
			}
			return at
		},
		tick: func(interval time.Duration) (<-chan time.Time, func()) {
			return this.ticks, func() {}
		},
	}
}

// awaitStart waits for the loop's first read of the clock, the one that puts
// its followers on their grid. Without it a first tick can be sent before the
// loop got that far, the loop reads the instant of that tick as its start and
// the whole grid sits one cadence too late.
func (this *followTestClock) awaitStart(t *testing.T) {
	t.Helper()
	select {
	case <-this.read:
	case <-time.After(10 * time.Second):
		t.Fatal("the follow loop never read the clock it was given")
	}
}

// tick moves the clock to at and hands the loop one tick, returning once the
// loop has read that instant - one read per tick, so the token below is always
// this tick's. The tick channel is unbuffered, so the next send returns only
// after the refresh of this one is over, which is what lets a test count
// fetches without polling. A repeated tick at the same instant is therefore
// the way to wait for the last refresh: it is never due.
func (this *followTestClock) tick(t *testing.T, at time.Time) {
	t.Helper()
	this.mux.Lock()
	this.now = at
	this.mux.Unlock()
	select {
	case this.ticks <- at:
	case <-time.After(10 * time.Second):
		t.Fatal("the follow loop did not take the tick")
	}
	select {
	case <-this.read:
	case <-time.After(10 * time.Second):
		t.Fatal("the follow loop never read the clock after the tick")
	}
}

// TestFollowDueStaysOnTheFollowersOwnGrid: the due instant is advanced by one
// cadence from the previous due instant, not from the moment a tick was
// received. Advancing it from the receipt puts it a hair after the next tick
// whenever a tick was a hair late, and a follower whose cadence is the tick
// interval - the default of an environment with one following source - then
// loses about every second refresh.
func TestFollowDueStaysOnTheFollowersOwnGrid(t *testing.T) {
	const every = time.Minute
	base := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	at := func(periods float64) time.Time {
		return base.Add(time.Duration(periods * float64(every)))
	}

	nextDue := at(1) //what the loop start sets: one cadence after it began
	var refreshed []time.Time
	//receipts jittering around the grid, the way a ticker delivers them
	for _, periods := range []float64{0, 0.99, 2.02, 2.98, 4.01} {
		now := at(periods)
		due, next := followDue(nextDue, now, every, every)
		nextDue = next
		if due {
			refreshed = append(refreshed, now)
		}
	}
	//one refresh per period, and none before the first due instant
	want := []time.Time{at(0.99), at(2.02), at(2.98), at(4.01)}
	if len(refreshed) != len(want) {
		t.Fatalf("refreshed at %v, want one per period at %v", refreshed, want)
	}
	for i, expected := range want {
		if !refreshed[i].Equal(expected) {
			t.Errorf("refresh %d was at %v, want %v", i, refreshed[i], expected)
		}
	}
}

// TestFollowDueDoesNotQueueCatchUpRefreshes: a fetch that took several
// cadences must not leave a run of immediately due refreshes behind it, which
// would ask the wrapper for the same tail several times over.
func TestFollowDueDoesNotQueueCatchUpRefreshes(t *testing.T) {
	const every = time.Minute
	base := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	nextDue := base.Add(every)

	//the refresh of the first due tick took five cadences
	late := base.Add(6 * every)
	due, nextDue := followDue(nextDue, late, every, every)
	if !due {
		t.Fatal("a follower five cadences past its due instant has to be due")
	}
	if want := late.Add(every); !nextDue.Equal(want) {
		t.Fatalf("the next due instant is %v, want one cadence after now %v: the missed ones are dropped, not queued", nextDue, want)
	}
	//and the very next tick is not due again
	if due, _ = followDue(nextDue, late.Add(every/4), every, every); due {
		t.Error("the tick right after a catch-up is due again, so the missed cadences were queued after all")
	}
}

// TestAFollowLoopRefreshesOncePerTickAtTheDefaultCadence: the end to end form
// of the two tests above, on a clock the test owns. An environment with one
// following source ticks at exactly that source's follow_every, which is the
// case the receipt-based schedule got wrong: measured against the wall clock
// it refreshed about every 1.6 cadences.
func TestAFollowLoopRefreshesOncePerTickAtTheDefaultCadence(t *testing.T) {
	const id = "env-follow-grid"
	const every = time.Minute
	const ticks = 40

	fetcher := &fakeFetcher{pointsFunc: func(index int, start time.Time, end time.Time) ([]dataset.Point, error) {
		return []dataset.Point{{Unix: end.Unix() - 1, Value: 11}, {Unix: end.Unix(), Value: 22}}, nil
	}}
	env := testEnvironment(id, followingChannel(id, "1m"))
	env.Owner = "owner-follow"
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, newFakeHistoryJobs(), &fakePublisher{})
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }
	base := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	clock := newFollowTestClock(base)
	rt.followClock = clock.hook()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)
	clock.awaitStart(t)

	//jitter well inside half a tick, alternating early and late, so that a
	//schedule advanced from the receipt skips a tick whenever this drops
	jitter := []time.Duration{2 * time.Second, -3 * time.Second, 5 * time.Second, -time.Second, 7 * time.Second, 0}
	var last time.Time
	for i := 1; i <= ticks; i++ {
		last = base.Add(time.Duration(i)*every + jitter[i%len(jitter)])
		clock.tick(t, last)
	}
	//a repeat of the last instant is never due, so it only waits for the last
	//refresh to have finished
	clock.tick(t, last)

	//one initial load plus one refresh per tick, exactly
	if got, want := fetcher.callCount(), ticks+1; got != want {
		t.Errorf("the loop fetched %d times over %d ticks, want %d: one load and one refresh per tick", got, ticks, want)
	}
}

// TestAFollowingReplayStopsHoldingWhenItsUpstreamStops: the hold between two
// refreshes must not become an unbounded hold. A source whose import died
// would otherwise publish its last measurement forever, and nothing about the
// reading would say that it is a week old.
func TestAFollowingReplayStopsHoldingWhenItsUpstreamStops(t *testing.T) {
	points := []dataset.Point{{Unix: 1000, Value: 100}, {Unix: 1800, Value: 400}}
	for _, tc := range []struct {
		name        string
		followEvery string
		at          int64
		want        bool
	}{
		{"one cadence past the newest point", "1m", 1800 + 60, true},
		{"exactly at the bound of three cadences", "1m", 1800 + 180, true},
		{"one second past the bound", "1m", 1800 + 181, false},
		{"an unset follow_every uses the default cadence", "", 1800 + 3*30*60, true},
		{"and its bound too", "", 1800 + 3*30*60 + 1, false},
	} {
		source := replaySource(domain.ResampleHold, domain.AnchorOriginal)
		source.Follow = true
		source.FollowEvery = tc.followEvery
		value, playable := replayValue(source, points, 0, time.Unix(tc.at, 0), 30)
		if playable != tc.want {
			t.Errorf("%s: playable is %v at %d, want %v", tc.name, playable, tc.at, tc.want)
			continue
		}
		if playable && value != 400 {
			t.Errorf("%s: held %v, want the newest measurement 400", tc.name, value)
		}
	}
}

// TestADistributingFollowerStaysSilentPastItsLastPoint: "distribute" hands out
// the share of a slot a tick stands for, so holding the last sample past the
// end of the series would keep integrating that sample's quantity into every
// slot after it - an energy nobody measured, growing for as long as the
// upstream stays away. There is nothing new to distribute, so it stays silent.
func TestADistributingFollowerStaysSilentPastItsLastPoint(t *testing.T) {
	points := []dataset.Point{{Unix: 1000, Value: 100}, {Unix: 1800, Value: 400}}
	source := replaySource(domain.ResampleDistribute, domain.AnchorOriginal)
	source.Follow = true
	source.FollowEvery = "1m"

	//well inside the hold a "hold" source would still answer over
	if _, playable := replayValue(source, points, 0, time.Unix(1830, 0), 30); playable {
		t.Error("a distributing follower kept playing past its last point")
	}
	//inside its own range it distributes as it always did
	value, playable := replayValue(source, points, 0, time.Unix(1200, 0), 30)
	if !playable {
		t.Fatal("a distributing follower has to play inside its own range")
	}
	if want := 100 * 30.0 / 800; value != want {
		t.Errorf("the distributed share is %v, want %v", value, want)
	}
	//and the same instant is held by a "hold" source, so the difference is the
	//resample mode and not the bound
	holding := replaySource(domain.ResampleHold, domain.AnchorOriginal)
	holding.Follow = true
	holding.FollowEvery = "1m"
	if _, playable = replayValue(holding, points, 0, time.Unix(1830, 0), 30); !playable {
		t.Error("a holding follower has to keep playing there, or this test proves nothing about distribute")
	}
}

// TestAStaleFollowingSourceIsReportedOnceAndOnRecovery: a source that stops
// delivering goes silent, and silence without a line in the log is the one
// failure nobody notices. It is worth exactly one line, though: an upstream
// that is away for a week must not write one per refresh.
func TestAStaleFollowingSourceIsReportedOnceAndOnRecovery(t *testing.T) {
	const id = "env-follow-stale"
	const staleMessage = "a following dataset source has gone stale"
	const recoveredMessage = "a following dataset source is delivering again"

	var delivering atomic.Bool
	fetcher := &fakeFetcher{pointsFunc: func(index int, start time.Time, end time.Time) ([]dataset.Point, error) {
		if index == 0 {
			return []dataset.Point{{Unix: end.Unix() - 1, Value: 11}, {Unix: end.Unix(), Value: 22}}, nil
		}
		if !delivering.Load() {
			return nil, nil //the ordinary answer of a source that has nothing new
		}
		return []dataset.Point{{Unix: end.Unix() + 3600, Value: 99}}, nil
	}}
	log := recordLog(t)
	env := testEnvironment(id, followingChannel(id, "1m"))
	env.Owner = "owner-follow"
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, newFakeHistoryJobs(), &fakePublisher{})
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }
	base := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	clock := newFollowTestClock(base)
	rt.followClock = clock.hook()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)
	clock.awaitStart(t)

	//two empty refreshes are a source that publishes less often than it is
	//followed, which is ordinary and silent
	for i := 1; i <= 2; i++ {
		clock.tick(t, base.Add(time.Duration(i)*time.Minute))
	}
	clock.tick(t, base.Add(2*time.Minute))
	if got := log.count(staleMessage); got != 0 {
		t.Errorf("two empty refreshes were reported %d times, expected silence", got)
	}

	for i := 3; i <= 12; i++ {
		clock.tick(t, base.Add(time.Duration(i)*time.Minute))
	}
	clock.tick(t, base.Add(12*time.Minute))
	if got := log.count(staleMessage); got != 1 {
		t.Errorf("ten empty refreshes wrote %d stale lines, want exactly one", got)
	}
	if got := log.count("channel=ch-follow"); got == 0 {
		t.Error("the stale line does not name the channel it is about")
	}

	delivering.Store(true)
	clock.tick(t, base.Add(13*time.Minute))
	clock.tick(t, base.Add(13*time.Minute))
	if got := log.count(recoveredMessage); got != 1 {
		t.Errorf("the recovery was reported %d times, want exactly one", got)
	}
	if got := log.count(staleMessage); got != 1 {
		t.Errorf("the stale line was written %d times in total, want exactly one", got)
	}
}

// TestAFailedRecoveryReloadIsProbedAgain: the loop reloads the environment for
// a source whose initial load failed, and that reload can fail on its own -
// the definition read is a database call. The source may not be left with
// points but no binding then, which is what storing the probe's points into
// the old generation did: every later tick took the tail path, the channel
// never got a binding and nothing ever probed again.
func TestAFailedRecoveryReloadIsProbedAgain(t *testing.T) {
	bypassTheFollowTickFloor(t)
	const id = "env-follow-reload-retry"
	fetcher := &fakeFetcher{pointsFunc: func(index int, start time.Time, end time.Time) ([]dataset.Point, error) {
		if index == 0 {
			return nil, errors.New("the wrapper is unavailable")
		}
		return []dataset.Point{
			{Unix: end.Unix() - 2, Value: 11}, {Unix: end.Unix() - 1, Value: 22}, {Unix: end.Unix(), Value: 99},
		}, nil
	}}
	env := testEnvironment(id, followingChannel(id, "20ms"))
	env.Owner = "owner-follow"
	environments := newFakeEnvironments(env)
	//the definition read of the first reload fails, the way a mongo blip looks,
	//and every read is slow enough that a good many ticks fall inside a reload
	environments.failGets(1, errors.New("the definition store is unavailable"))
	environments.getDelay = 300 * time.Millisecond
	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), environments, newFakeStates(), nil, newFakeHistoryJobs(), publisher)
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	if !waitFor(20*time.Second, func() bool { return len(publisher.forDevice(deviceRefOf(id))) > 0 }) {
		t.Fatalf("the channel never published after the first reload failed: %d fetch calls, %d definition reads, %d stored points",
			fetcher.callCount(), environments.getCount(), len(seriesOf(t, rt, id, "ch-follow")))
	}
	//exactly two: the reload that failed and the one that worked. Without the
	//pending flag every tick of the loop would spawn another one while the
	//first is still running, at twenty definition reads a second.
	if got := environments.getCount(); got != 2 {
		t.Errorf("the definition was read %d times, want 2 - the failed reload and the one after it", got)
	}
}

// TestStopWaitsForTheReloadAFollowLoopSpawned: the reload runs on a goroutine
// of its own, since Reload waits for the runners the loop spawning it is one
// of. Nothing may outlive Stop, so Stop waits for that goroutine - with the
// lifecycle mutex released, which is what the goroutine blocks on.
func TestStopWaitsForTheReloadAFollowLoopSpawned(t *testing.T) {
	bypassTheFollowTickFloor(t)
	const id = "env-follow-reload-stop"
	probing := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	openTheProbe := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(openTheProbe)

	var probeOnce sync.Once
	fetcher := &fakeFetcher{pointsFunc: func(index int, start time.Time, end time.Time) ([]dataset.Point, error) {
		if index == 0 {
			return nil, errors.New("the wrapper is unavailable")
		}
		if index == 1 {
			//the probe hangs until the test lets it go, and deliberately
			//ignores the cancelled context: a probe that answers while the
			//runtime shuts down is exactly the one whose reload must not
			//outlive Stop
			probeOnce.Do(func() { close(probing) })
			<-release
		}
		return []dataset.Point{
			{Unix: end.Unix() - 2, Value: 11}, {Unix: end.Unix() - 1, Value: 22}, {Unix: end.Unix(), Value: 99},
		}, nil
	}}
	log := recordLog(t)
	env := testEnvironment(id, followingChannel(id, "20ms"))
	env.Owner = "owner-follow"
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, newFakeHistoryJobs(), &fakePublisher{})
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	select {
	case <-probing:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop never probed the source whose initial load failed")
	}

	stopped := make(chan struct{})
	go func() {
		rt.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a fetch of the follow loop was still in flight")
	case <-time.After(300 * time.Millisecond):
	}

	//the probe answers, the loop spawns the reload and ends; the reload then
	//waits for the lifecycle mutex Stop holds
	openTheProbe()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("Stop never returned")
	}

	//the spawned reload has run by the time Stop returns: it finds a stopped
	//runtime and says so. Without the wait it would still be queued here.
	if got := log.count("the runtime is not running, ignoring the reload"); got != 1 {
		t.Errorf("the spawned reload reported itself %d times by the time Stop returned, want once: it outlived Stop", got)
	}
	rt.mux.RLock()
	live := rt.envs[id]
	rt.mux.RUnlock()
	if live != nil {
		live.mux.Lock()
		pending := live.reloadPending
		live.mux.Unlock()
		if pending {
			t.Error("the reload is still marked pending after Stop returned")
		}
	}
}

// TestASourceThatIsAlreadyStaleAtStartIsReportedAtOnce: a series whose newest
// point is older than the hold bound when the loop starts gets its warning at
// start, not three empty refreshes later - the channel is silent from the
// first tick, and the log has to say why right then.
func TestASourceThatIsAlreadyStaleAtStartIsReportedAtOnce(t *testing.T) {
	const id = "env-follow-stale-start"
	const staleMessage = "a following dataset source has gone stale"
	fetcher := &fakeFetcher{pointsFunc: func(index int, start time.Time, end time.Time) ([]dataset.Point, error) {
		if index == 0 {
			old := end.Add(-24 * time.Hour).Unix()
			return []dataset.Point{{Unix: old - 60, Value: 11}, {Unix: old, Value: 22}}, nil
		}
		return nil, nil
	}}
	log := recordLog(t)
	env := testEnvironment(id, followingChannel(id, "1m"))
	env.Owner = "owner-follow"
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, newFakeHistoryJobs(), &fakePublisher{})
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }
	base := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	clock := newFollowTestClock(base)
	rt.followClock = clock.hook()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)
	clock.awaitStart(t)
	if got := log.count(staleMessage); got != 1 {
		t.Errorf("a source stale at start was reported %d times before the first refresh, want exactly one", got)
	}
	clock.tick(t, base.Add(time.Minute))
	clock.tick(t, base.Add(time.Minute))
	if got := log.count(staleMessage); got != 1 {
		t.Errorf("the stale line was written %d times after the first refresh, want exactly one", got)
	}
}
