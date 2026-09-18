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
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// errNoFallbackRows is what the wrapper answers for a fallback selecting rows
// that are not there: fewer than two measurements is a refusal of Fetch.
var errNoFallbackRows = errors.New("the window holds 0 usable measurements, replay needs at least 2")

// The substitute series of the tests below: ten minute points over the whole
// range of gapPoints, so it covers the 6600 s hole gapPoints has between 600
// and 7200. Its values are far away from the main series', so a reading says
// which of the two it came from.
var neighbourPoints = func() []dataset.Point {
	points := make([]dataset.Point, 0, 14)
	for second := int64(0); second <= 7800; second += 600 {
		points = append(points, dataset.Point{Unix: second, Value: 100 + float64(second)/600})
	}
	return points
}()

// a substitute with a hole of its own across the middle of the main one: 3000
// to 7200 is 4200 s and wider than the bound of an hour
var holedNeighbourPoints = []dataset.Point{
	{Unix: 0, Value: 100}, {Unix: 3000, Value: 105},
	{Unix: 7200, Value: 112}, {Unix: 7800, Value: 113},
}

// fallbackSource is gapSource declaring a neighbouring station to fall back on.
func fallbackSource(resample domain.ResampleMode, maxGap string) domain.DatasetSource {
	source := gapSource(resample, maxGap)
	source.Origin = domain.OriginExport
	source.Ref = "export-1"
	source.Column = "temperature"
	source.Window = "7d"
	source.Filters = []domain.DatasetFilter{{Column: "station_id", Value: "02932"}}
	source.Fallback = &domain.DatasetFallback{
		Filters: []domain.DatasetFilter{{Column: "station_id", Value: "01048"}},
	}
	return source
}

func TestAFallbackSeriesCoversAGapWiderThanMaxGap(t *testing.T) {
	//3900 s into the series is the middle of the 6600 s hole, and the substitute
	//has a point every ten minutes there
	const inside = 3900
	for _, tc := range []struct {
		mode domain.ResampleMode
		want float64
	}{
		//between the substitute's 106 at 3600 and its 107 at 4200
		{domain.ResampleLinear, 106.5},
		{domain.ResampleHold, 106},
		//the share of the 600 s slot at 3600 that a 30 s tick stands for
		{domain.ResampleDistribute, 106 * 30 / 600.0},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			series := replaySeries{points: gapPoints, fallback: neighbourPoints}
			value, gap, covered, playable := replayWithFallback(
				fallbackSource(tc.mode, "1h"), series, 0, time.Unix(inside, 0), 30)
			if !playable {
				t.Fatal("the substitute covers this instant, so the source is not silent")
			}
			if !covered {
				t.Error("the value came from the substitute and has to be reported as such")
			}
			if value != tc.want {
				t.Errorf("expected the substitute's %v, got %v", tc.want, value)
			}
			//the gap is still reported: the main series has a hole here whether
			//or not something else fills it
			if gap.Seconds != 6600 || gap.StartUnix != 600 {
				t.Errorf("expected the main series' gap of 6600 s opening at 600, got %+v", gap)
			}
		})
	}
}

func TestAGapNarrowerThanMaxGapDoesNotReadTheFallback(t *testing.T) {
	//an hour and 50 minutes bounds nothing here: the hole is 6600 s and the
	//bound 6600 s, so the replay bridges it itself
	series := replaySeries{points: gapPoints, fallback: neighbourPoints}
	value, gap, covered, playable := replayWithFallback(
		fallbackSource(domain.ResampleLinear, "6600s"), series, 0, time.Unix(3900, 0), 30)
	if !playable {
		t.Fatal("a hole no wider than the bound is bridged as it always was")
	}
	if covered || gap.Seconds != 0 {
		t.Errorf("no gap means no substitute was read, got covered %v and gap %+v", covered, gap)
	}
	//halfway between the main series' 20 at 600 and its 80 at 7200
	if value != 50 {
		t.Errorf("expected the interpolated 50 of the source's own series, got %v", value)
	}
}

func TestAFallbackWithAGapOfItsOwnIsSilent(t *testing.T) {
	//the substitute is not a second chance to bridge: its own hole is measured
	//against the same bound, and past it the source says nothing, as it did
	//before there was a substitute
	for _, mode := range []domain.ResampleMode{
		domain.ResampleLinear, domain.ResampleHold, domain.ResampleDistribute,
	} {
		t.Run(string(mode), func(t *testing.T) {
			series := replaySeries{points: gapPoints, fallback: holedNeighbourPoints}
			value, gap, covered, playable := replayWithFallback(
				fallbackSource(mode, "1h"), series, 0, time.Unix(3900, 0), 30)
			if playable {
				t.Fatalf("both series have a hole here, so nothing is played, got %v", value)
			}
			if covered {
				t.Error("nothing was covered")
			}
			if gap.Seconds != 6600 {
				t.Errorf("the reported gap is the main series', got %+v", gap)
			}
		})
	}
}

func TestThereIsNoFallbackOutsideTheRangeOfTheMainSeries(t *testing.T) {
	//max_gap is about the middle of a series and not its edges, and so is the
	//series that stands in for it: before the first point and after the last one
	//the source is silent, even where the substitute has measurements
	source := fallbackSource(domain.ResampleLinear, "1h")
	source.Anchor = domain.AnchorOriginal
	wider := append([]dataset.Point{{Unix: -600, Value: 99}}, neighbourPoints...)
	wider = append(wider, dataset.Point{Unix: 8400, Value: 199})
	series := replaySeries{points: gapPoints, fallback: wider}
	for _, second := range []int64{-300, 8100} {
		value, gap, covered, playable := replayWithFallback(source, series, 0, time.Unix(second, 0), 30)
		if playable || covered {
			t.Errorf("second %d: outside its own range the source stays silent, got %v", second, value)
		}
		if gap.Seconds != 0 {
			t.Errorf("second %d: the end of a series is not a gap, got %+v", second, gap)
		}
	}
}

func TestAPointOfTheMainSeriesWinsOverTheFallback(t *testing.T) {
	//the points bounding the hole are measurements, so they answer even though
	//the substitute has a point of its own at the same instant
	series := replaySeries{points: gapPoints, fallback: neighbourPoints}
	for _, tc := range []struct {
		second int64
		want   float64
	}{{600, 20}, {7200, 80}} {
		for _, mode := range []domain.ResampleMode{domain.ResampleLinear, domain.ResampleHold} {
			value, _, covered, playable := replayWithFallback(
				fallbackSource(mode, "1h"), series, 0, time.Unix(tc.second, 0), 30)
			if !playable || covered {
				t.Errorf("%s at %d: a measurement of the source itself wins", mode, tc.second)
			}
			if value != tc.want {
				t.Errorf("%s at %d: expected the measured %v, got %v", mode, tc.second, tc.want, value)
			}
		}
	}

	//distribute measures the slot rather than the instant, so the point at 600
	//opens a 6600 s slot that is itself past the bound - the substitute answers
	//there, exactly as it does in the middle of the hole
	value, gap, covered, playable := replayWithFallback(
		fallbackSource(domain.ResampleDistribute, "1h"), series, 0, time.Unix(600, 0), 30)
	if !playable || !covered {
		t.Fatalf("distribute at 600: the oversized slot is part of the gap, got playable %v covered %v", playable, covered)
	}
	if gap.Seconds != 6600 {
		t.Errorf("distribute at 600: expected the slot of 6600 s, got %d", gap.Seconds)
	}
	//the substitute's 101 at 600, whose slot is 600 s, shared out over a 30 s tick
	if want := 101 * 30 / 600.0; value != want {
		t.Errorf("distribute at 600: expected the substitute's share %v, got %v", want, value)
	}
}

func TestADeclaredFallbackWithoutPointsChangesNothing(t *testing.T) {
	//a substitute whose load failed leaves the source exactly as it was: silent
	//inside its hole, and playable everywhere else
	series := replaySeries{points: gapPoints}
	source := fallbackSource(domain.ResampleLinear, "1h")
	if _, gap, covered, playable := replayWithFallback(source, series, 0, time.Unix(3900, 0), 30); playable || covered || gap.Seconds != 6600 {
		t.Errorf("expected the unchanged silence of a gap, got playable %v covered %v gap %+v", playable, covered, gap)
	}
	//a single point is no series either: it has no slot and no span to read
	series.fallback = []dataset.Point{{Unix: 3900, Value: 7}}
	if _, _, covered, playable := replayWithFallback(source, series, 0, time.Unix(3900, 0), 30); playable || covered {
		t.Error("one point is not a series a replay can read")
	}
}

func TestTheScaleHoldsForTheFallbackToo(t *testing.T) {
	//the substitute stands in for a value of the main series at this instant, so
	//it carries the corrections that value would have carried. Cumulative is not
	//among them: validation refuses a meter with a fallback, because a second
	//meter's register is not this meter's count.
	source := fallbackSource(domain.ResampleHold, "1h")
	source.Scale = 10
	series := replaySeries{points: gapPoints, fallback: neighbourPoints}
	//one whole loop of the 7800 s series behind it, then 3900 s into the second:
	//the substitute is read at the instant inside the loop, not at the wall clock
	value, _, covered, playable := replayWithFallback(source, series, 0, time.Unix(7800+3900, 0), 30)
	if !playable || !covered {
		t.Fatal("the substitute covers the middle of the hole in the second loop too")
	}
	if want := 106 * 10.0; value != want {
		t.Errorf("expected the substitute's 106 scaled by ten, got %v", value)
	}
}

func TestTheBackfillReadsTheSameFallback(t *testing.T) {
	//a window rebuilt after the fact has to carry the rows the live path
	//published, substitute included
	runtime := &Runtime{}
	gen := &generation{gapReported: map[string]gapReport{}, timeline: newTimelineIndex(domain.Environment{})}
	source := fallbackSource(domain.ResampleHold, "1h")
	channel := backfillChannel{channel: domain.Channel{
		Id:     "ch-1",
		Source: domain.Source{Kind: domain.SourceDataset, Dataset: &source},
	}}
	series := replaySeries{points: gapPoints, fallback: neighbourPoints}
	counter := 0.0
	value, playable := runtime.backfillValue(gen, channel, series, 0, time.Unix(3900, 0), &counter, false, 30)
	if !playable {
		t.Fatal("the backfill went silent where the live path reads the substitute")
	}
	if value != 106 {
		t.Errorf("expected the substitute's 106, got %v", value)
	}
	live, _, _, _ := replayWithFallback(source, series, 0, time.Unix(3900, 0), 30)
	if value != live {
		t.Errorf("the backfill and the live tick have to agree bit for bit, got %v and %v", value, live)
	}
}

func TestACoveredGapIsReportedAndSoIsLosingTheCover(t *testing.T) {
	gen := &generation{gapReported: map[string]gapReport{}}
	gap := replayGap{StartUnix: 600, Seconds: 6600}
	if !reportGap(gen, "ch-1", gap, "1h", true, true) {
		t.Fatal("the first sighting of a covered gap is reported")
	}
	if reportGap(gen, "ch-1", gap, "1h", true, true) {
		t.Error("a covered gap is reported once, not once per tick")
	}
	//the substitute has a hole of its own halfway through: the channel falls
	//silent, which the line claiming coverage would otherwise hide
	if !reportGap(gen, "ch-1", gap, "1h", true, false) {
		t.Error("losing the cover inside one gap is an event of its own")
	}
	if reportGap(gen, "ch-1", gap, "1h", true, false) {
		t.Error("and then it is reported once too")
	}
	if !reportGap(gen, "ch-1", gap, "1h", true, true) {
		t.Error("getting the cover back is an event again")
	}
}

// Both series are loaded the same way: one window, one column, one export -
// only the filters differ. A refresh that asked for a different window would
// read the two on different terms.
func TestBothSeriesOfAFallbackSourceAreFetchedAlike(t *testing.T) {
	envId := "env-fallback-load"
	source := fallbackSource(domain.ResampleHold, "1h")
	source.Anchor = domain.AnchorLoop
	channel := domain.Channel{
		Id: "ch-fallback", Name: "weather", Direction: domain.Sensor,
		ExternalRef: serviceRefOf(envId) + "-fallback", IntervalSeconds: 1,
		Source: domain.Source{Kind: domain.SourceDataset, Dataset: &source},
	}
	env := testEnvironment(envId, channel)
	env.Owner = "owner-42"

	now := time.Now().Unix()
	main := []dataset.Point{{Unix: now - 7200, Value: 11}, {Unix: now, Value: 22}}
	substitute := []dataset.Point{{Unix: now - 7200, Value: 111}, {Unix: now, Value: 222}}
	fetcher := &fakeFetcher{pointsFunc: func(index int, start time.Time, end time.Time) ([]dataset.Point, error) {
		if index == 0 {
			return main, nil
		}
		return substitute, nil
	}}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, newFakeHistoryJobs(), &fakePublisher{})
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }

	loaded := rt.loadSeries(context.Background(), env)
	if len(fetcher.calls) != 2 {
		t.Fatalf("expected one fetch per series, got %d", len(fetcher.calls))
	}
	main0, fallback0 := fetcher.calls[0], fetcher.calls[1]
	for _, field := range []string{"token", "export", "column", "window"} {
		if main0[field] != fallback0[field] {
			t.Errorf("both series are read on the same terms, %s differs: %q and %q", field, main0[field], fallback0[field])
		}
	}
	if main0["window"] != "168h0m0s" {
		t.Errorf("expected the source's 7d window, got %q", main0["window"])
	}
	if main0["filters"] != "station_id=02932" {
		t.Errorf("the main series is narrowed by the source's own filters, got %q", main0["filters"])
	}
	if fallback0["filters"] != "station_id=01048" {
		t.Errorf("the substitute is narrowed by the fallback's filters, got %q", fallback0["filters"])
	}
	series := loaded["ch-fallback"]
	if len(series.points) != 2 || series.points[0].Value != 11 {
		t.Errorf("the source's own series is the main one, got %v", series.points)
	}
	if len(series.fallback) != 2 || series.fallback[0].Value != 111 {
		t.Errorf("the substitute is stored beside it, got %v", series.fallback)
	}
}

// A substitute that cannot be loaded costs the coverage and nothing else: the
// channel itself keeps playing, since its own series is there.
func TestAnUnloadableFallbackLeavesTheSourcePlaying(t *testing.T) {
	envId := "env-fallback-broken"
	source := fallbackSource(domain.ResampleHold, "1h")
	source.Anchor = domain.AnchorLoop
	channel := domain.Channel{
		Id: "ch-fallback", Name: "weather", Direction: domain.Sensor,
		ExternalRef: serviceRefOf(envId) + "-fallback", IntervalSeconds: 1,
		Source: domain.Source{Kind: domain.SourceDataset, Dataset: &source},
	}
	env := testEnvironment(envId, channel)
	env.Owner = "owner-42"

	now := time.Now().Unix()
	main := []dataset.Point{{Unix: now - 7200, Value: 11}, {Unix: now, Value: 22}}
	fetcher := &fakeFetcher{
		pointsSeq: [][]dataset.Point{main, nil},
		errSeq:    []error{nil, errNoFallbackRows},
	}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, newFakeHistoryJobs(), &fakePublisher{})
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }

	loaded := rt.loadSeries(context.Background(), env)
	series := loaded["ch-fallback"]
	if len(series.points) != 2 {
		t.Fatalf("the source's own series has to be loaded, got %v", series.points)
	}
	if series.fallback != nil {
		t.Errorf("a substitute that did not load is absent rather than half there, got %v", series.fallback)
	}
	//and the channel still binds, which is what keeps it running
	gen := newGeneration(env, loaded)
	if len(gen.sensors) != 1 {
		t.Errorf("expected the channel to stay bound, got %d ticking channels", len(gen.sensors))
	}
}

// A following source refreshes both of its series, each from its own rows.
func TestAFollowingSourceFollowsItsFallbackToo(t *testing.T) {
	source := fallbackSource(domain.ResampleHold, "1h")
	source.Anchor = domain.AnchorOriginal
	source.Follow = true
	source.FollowEvery = "5m"
	def := testEnvironment("env-follow-fallback", domain.Channel{
		Id: "ch-fallback", Name: "weather", Direction: domain.Sensor,
		ExternalRef: "svc", IntervalSeconds: 1,
		Source: domain.Source{Kind: domain.SourceDataset, Dataset: &source},
	})
	followers := followersOf(def, map[string]replaySeries{"ch-fallback": {points: gapPoints}})
	if len(followers) != 2 {
		t.Fatalf("expected the source and its substitute to follow, got %d", len(followers))
	}
	if followers[0].isFallback || !followers[1].isFallback {
		t.Fatalf("expected the main series first and the substitute second, got %+v", followers)
	}
	if followers[0].seriesId != followers[1].seriesId {
		t.Error("both series of one source are stored under the id of that source")
	}
	if followers[0].every != followers[1].every || followers[0].window != followers[1].window {
		t.Error("both series are refreshed on the same cadence over the same window")
	}
	if followers[1].dataset.Filters[0].Value != "01048" {
		t.Errorf("the substitute is refreshed from its own rows, got %+v", followers[1].dataset.Filters)
	}

	//and each writes into its own half of the stored entry
	gen := newGeneration(def, map[string]replaySeries{"ch-fallback": {points: gapPoints}})
	followers[1].store(gen, neighbourPoints)
	if got := gen.series["ch-fallback"]; len(got.points) != len(gapPoints) || len(got.fallback) != len(neighbourPoints) {
		t.Errorf("a refresh of the substitute must not touch the main series, got %d and %d points", len(got.points), len(got.fallback))
	}
	if len(followers[0].stored(gen)) != len(gapPoints) {
		t.Error("the main follower reads the main series")
	}
	if len(followers[1].stored(gen)) != len(neighbourPoints) {
		t.Error("the fallback follower reads the substitute")
	}
}

// The live tick reads both series and reports the gap once, which is what ties
// the arithmetic above to the channel that publishes it.
func TestALiveTickPublishesTheFallbackAndReportsTheGapOnce(t *testing.T) {
	const envId = "env-fallback-tick"
	source := fallbackSource(domain.ResampleHold, "1h")
	def := testEnvironment(envId, datasetChannel(envId, source))
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, newFakeHistoryJobs(), &fakePublisher{})
	//a dataset channel binds only with its series loaded, so it goes in here
	gen := newGeneration(def, map[string]replaySeries{
		"ch-1": {points: gapPoints, fallback: neighbourPoints},
	})
	if len(gen.sensors) != 1 {
		t.Fatalf("expected exactly one ticking channel, got %d", len(gen.sensors))
	}
	env := &environment{id: envId, gen: gen, state: repo.RuntimeState{EnvironmentId: envId}}
	binding := gen.sensors[0]
	//the loop anchor is set on the first dispatch, so the replay starts at the
	//first point of the series and 3900 s of the window are 3900 s into it
	start := time.Unix(1_800_000_000, 0)

	var got []interface{}
	send := func(value interface{}) { got = append(got, value) }
	rt.dispatch(env, gen, binding, nil, send, true, start)
	rt.dispatch(env, gen, binding, nil, send, true, start.Add(3900*time.Second))
	rt.dispatch(env, gen, binding, nil, send, true, start.Add(3930*time.Second))

	if len(got) != 3 {
		t.Fatalf("every tick has a value, either the source's or the substitute's, got %v", got)
	}
	if got[0] != 10.0 {
		t.Errorf("the first tick is the source's own first point, got %v", got[0])
	}
	if got[1] != 106.0 || got[2] != 106.0 {
		t.Errorf("inside the hole the substitute answers, got %v and %v", got[1], got[2])
	}
	env.mux.Lock()
	report, reported := gen.gapReported["ch-1"]
	env.mux.Unlock()
	if !reported {
		t.Fatal("a gap a substitute covers is still a gap and is reported")
	}
	if report.startUnix != gapPoints[1].Unix || !report.covered {
		t.Errorf("expected the covered gap opening at 600, got %+v", report)
	}
}

// followingFallbackChannel is an export channel that follows and declares a
// neighbouring station, the shape both regressions below were found on.
func followingFallbackChannel(channelId string) domain.Channel {
	source := fallbackSource(domain.ResampleHold, "1h")
	source.Anchor = domain.AnchorOriginal
	source.Follow = true
	source.FollowEvery = "5m"
	return domain.Channel{
		Id: channelId, Name: channelId, Direction: domain.Sensor,
		ExternalRef: "svc", IntervalSeconds: 1,
		Source: domain.Source{Kind: domain.SourceDataset, Dataset: &source},
	}
}

// A source whose own series did not load has no substitute to follow. It used
// to have one, and that one recovered on every cadence: the probe loaded the
// neighbouring station, asked for a reload, the reload failed to load the main
// series again, and the next cadence started over - a full restart of every
// channel of the environment, once per follow_every, for as long as the
// upstream stayed empty.
func TestAFallbackDoesNotFollowASourceThatDidNotLoad(t *testing.T) {
	def := testEnvironment("env-fallback-unloaded", followingFallbackChannel("ch-fallback"))
	//what loadSeries leaves behind when the main fetch failed: no entry at all
	if followers := followersOf(def, map[string]replaySeries{}); len(followers) != 1 {
		t.Fatalf("expected only the source itself to follow, got %d followers", len(followers))
	}
	//and with the source's own series loaded the substitute follows again
	loaded := map[string]replaySeries{"ch-fallback": {points: gapPoints}}
	if followers := followersOf(def, loaded); len(followers) != 2 {
		t.Fatalf("expected the source and its substitute to follow, got %d followers", len(followers))
	}
}

// And a refresh of the substitute never asks for a reload on its own: nothing
// binds a substitute, so the points go straight into the entry the source's own
// series holds. A reload from here would repeat the load that failed.
func TestARefreshOfTheFallbackNeverTriggersAReload(t *testing.T) {
	def := testEnvironment("env-fallback-noreload", followingFallbackChannel("ch-fallback"))
	gen := newGeneration(def, map[string]replaySeries{"ch-fallback": {points: gapPoints}})
	env := &environment{id: def.Id, gen: gen, state: repo.RuntimeState{EnvironmentId: def.Id}}
	fetcher := &fakeFetcher{points: neighbourPoints}
	rt := &Runtime{fetcher: fetcher, ownerToken: func(string) (string, error) { return "token", nil }}

	followers := followersOf(def, gen.series)
	substitute := &followers[1]
	if !substitute.isFallback {
		t.Fatal("expected the second follower to be the substitute")
	}
	if reload := rt.refreshFollower(context.Background(), env, gen, substitute); reload {
		t.Error("a substitute recovering must not restart the environment")
	}
	env.mux.Lock()
	stored := gen.series["ch-fallback"]
	env.mux.Unlock()
	if len(stored.fallback) != len(neighbourPoints) {
		t.Errorf("the substitute has to land in the entry rather than wait for a reload, got %d points", len(stored.fallback))
	}
	if len(stored.points) != len(gapPoints) {
		t.Errorf("and the source's own series is untouched, got %d points", len(stored.points))
	}
	if len(fetcher.calls) != 1 {
		t.Errorf("expected one fetch for the substitute, got %d", len(fetcher.calls))
	}
}

// A neighbouring export that stays unreadable costs the coverage and nothing
// else: it is retried on the cadence, and it is reported once rather than on
// every one of them.
func TestAnUnreadableFallbackIsReportedOncePerSource(t *testing.T) {
	def := testEnvironment("env-fallback-quiet", followingFallbackChannel("ch-fallback"))
	gen := newGeneration(def, map[string]replaySeries{"ch-fallback": {points: gapPoints}})
	env := &environment{id: def.Id, gen: gen, state: repo.RuntimeState{EnvironmentId: def.Id}}
	fetcher := &fakeFetcher{errSeq: []error{errNoFallbackRows, errNoFallbackRows, errNoFallbackRows, errNoFallbackRows, errNoFallbackRows}}
	rt := &Runtime{fetcher: fetcher, ownerToken: func(string) (string, error) { return "token", nil }}
	followers := followersOf(def, gen.series)
	substitute := &followers[1]

	log := recordLog(t)
	for cadence := 0; cadence < 5; cadence++ {
		if reload := rt.refreshFollower(context.Background(), env, gen, substitute); reload {
			t.Fatalf("cadence %d: a substitute must not restart the environment", cadence)
		}
	}
	if got := log.count("unable to load the fallback series"); got != 1 {
		t.Errorf("expected one line for five cadences, got %d", got)
	}
	if got := len(fetcher.calls); got != 5 {
		t.Errorf("the retry itself stays on the cadence, expected 5 fetches, got %d", got)
	}
	//and once it answers, the line is armed again for the next outage
	fetcher.errSeq = nil
	fetcher.points = neighbourPoints
	if reload := rt.refreshFollower(context.Background(), env, gen, substitute); reload {
		t.Fatal("recovering is not a reload either")
	}
	env.mux.Lock()
	recovered := len(gen.series["ch-fallback"].fallback)
	env.mux.Unlock()
	if recovered != len(neighbourPoints) {
		t.Errorf("expected the substitute to land after it answered, got %d points", recovered)
	}
}
