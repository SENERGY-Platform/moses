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
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// platformFetcherWithPoints is a fetcher whose three points sit around now, so
// the replay of them is playable however long the test takes.
func platformFetcherWithPoints() *fakeFetcher {
	now := time.Now().Unix()
	return &fakeFetcher{points: []dataset.Point{
		{Unix: now - 7200, Value: 11}, {Unix: now - 3600, Value: 22}, {Unix: now, Value: 33},
	}}
}

// Starting an environment happens in two phases with two budgets: the series
// load, which may legitimately take minutes off the timescale-wrapper, and the
// state read behind it, which is an ordinary database call. Sharing one budget
// between them is what let a long fetch expire the context the state read then
// failed on - and a failed state read does not start the environment at all,
// with no retry until the next reload.
func TestTheSeriesLoadAndTheStateReadHaveSeparateBudgets(t *testing.T) {
	fetcher := platformFetcherWithPoints()
	env := testEnvironment("env-budget", platformChannel("env-budget"))
	env.Owner = "owner-42"
	states := newFakeStates()
	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), states, nil, publisher)
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer " + userId, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	fetch := fetcher.budgetAt(0)
	if !fetch.hasDeadline {
		t.Fatal("the fetch has to be bounded: an unbounded one would hold the lifecycle lock for as long as the wrapper likes")
	}
	if fetch.err != nil {
		t.Errorf("the fetch was handed a context that was already spent: %v", fetch.err)
	}
	if fetch.remaining <= storeTimeout {
		t.Errorf("the fetch got a budget of %v, which is the store budget rather than the series budget of %v",
			fetch.remaining, seriesLoadTimeout)
	}

	state := states.loadBudgetFor("env-budget")
	if !state.hasDeadline {
		t.Fatal("the state read has to be bounded")
	}
	if state.err != nil {
		t.Errorf("the state read ran on a context the series load had already spent: %v", state.err)
	}
	if state.remaining > storeTimeout {
		t.Errorf("the state read got a budget of %v, which is the series budget rather than its own %v",
			state.remaining, storeTimeout)
	}

	//and the environment really is running, which is what the failed state read
	//used to cost
	if !waitFor(5*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("the environment did not start")
	}
}

// The reported failure was a reload: it handed the budget of its definition
// read - storeTimeout, already partly spent - to the whole start, so a fetch
// longer than that expired it. Everything loaded after the fetch then ran on a
// dead context: the uploaded datasets of the other channels were dropped with a
// warning, and the state read failed outright.
func TestReloadGivesTheWholeSeriesLoadItsOwnBudget(t *testing.T) {
	csv := "Zeit;wert\n2026-01-05 00:00:00;1\n2026-01-05 00:00:01;2\n2026-01-05 00:00:02;4\n"
	store := &fakeRuntimeDatasets{
		meta:    repo.DatasetMeta{Id: "d1", Owner: "user-a", Name: "Lastgang", Timezone: "Europe/Berlin"},
		content: []byte(csv),
	}
	fetcher := platformFetcherWithPoints()

	envId := "env-reload-budget"
	//the platform channel first, the uploaded one behind it: that is the order
	//in which the second half of the failure appeared
	env := testEnvironment(envId,
		platformChannel(envId),
		datasetChannelWithColumn("ch-file", envId, "d1", "wert"),
	)
	env.Owner = "owner-42"
	env.ContextSources = map[string]domain.Source{
		"weather": {Kind: domain.SourceDataset, IntervalSeconds: 1,
			Dataset: &domain.DatasetSource{Origin: domain.OriginFile, Ref: "d1", Column: "wert",
				Resample: domain.ResampleHold, Anchor: domain.AnchorLoop}},
	}

	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), store, publisher)
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer " + userId, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	//so that the assertion below is about the generation the reload built and
	//not about what the first one had already published; Reload waits for the
	//old runners before starting the new ones
	if !waitFor(5*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("the environment did not start")
	}
	rt.Reload(envId)
	replayed := len(publisher.all())

	if got := fetcher.callCount(); got != 2 {
		t.Fatalf("expected one fetch per load, got %d", got)
	}
	reloadFetch := fetcher.budgetAt(1)
	if !reloadFetch.hasDeadline || reloadFetch.err != nil {
		t.Fatalf("the fetch of a reload has to run on a live bounded context, got %+v", reloadFetch)
	}
	if reloadFetch.remaining <= storeTimeout {
		t.Errorf("the reload squeezed the fetch into the budget of its definition read: %v left of %v",
			reloadFetch.remaining, seriesLoadTimeout)
	}

	//the second half: the uploaded dataset behind the platform channel is read
	//on the same live series budget instead of on whatever the fetch left over
	upload := store.lastContentBudget()
	if upload.err != nil {
		t.Errorf("the upload behind the platform channel was read on a spent context: %v", upload.err)
	}
	if upload.remaining <= storeTimeout {
		t.Errorf("the upload was read on a budget of %v rather than the series budget", upload.remaining)
	}

	//nothing was dropped: the uploaded channel behind the platform one plays in
	//the generation the reload built, which is what a spent context used to
	//cost it - silently, with a warning and an environment that runs short
	seen := map[float64]bool{}
	fromUpload := func() bool {
		for _, event := range publisher.all()[replayed:] {
			if number, ok := event.value.(float64); ok {
				seen[number] = true
			}
		}
		return seen[1] || seen[2] || seen[4]
	}
	if !waitFor(5*time.Second, fromUpload) {
		t.Errorf("the uploaded dataset behind the platform channel did not play after the reload, values seen: %v", seen)
	}
}

// fetchSeries is always called with the cache loadSeries built, but the
// platform branch returns before touching it - so a call site that forgot the
// cache would look correct in a platform test and panic on the first uploaded
// dataset of a real start. Nil costs the sharing and nothing else.
func TestFetchSeriesToleratesANilCache(t *testing.T) {
	csv := "Zeit;wert\n2026-01-05 00:00:00;1\n2026-01-05 00:00:01;2\n"
	store := &fakeRuntimeDatasets{
		meta:    repo.DatasetMeta{Id: "d1", Owner: "user-a", Name: "Lastgang", Timezone: "Europe/Berlin"},
		content: []byte(csv),
	}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), store, &fakePublisher{})
	source := replaySource(domain.ResampleHold, domain.AnchorLoop)

	points, err := rt.fetchSeries(context.Background(), "user-a", &source, nil)
	if err != nil {
		t.Fatalf("a nil cache has to cost the sharing, not the load: %v", err)
	}
	if len(points) != 2 || points[0].Value != 1 {
		t.Fatalf("unexpected points: %+v", points)
	}
}
