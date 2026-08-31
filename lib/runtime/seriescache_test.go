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

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// datasetChannelWithColumn is like datasetChannel but with an explicit id and
// column, so several channels of one environment can reference the same
// upload by different columns.
func datasetChannelWithColumn(id string, envId string, ref string, column string) domain.Channel {
	source := domain.DatasetSource{Origin: domain.OriginFile, Ref: ref, Column: column,
		Resample: domain.ResampleHold, Anchor: domain.AnchorLoop}
	return domain.Channel{
		Id: id, Name: id, Direction: domain.Sensor, ExternalRef: serviceRefOf(envId) + "-" + id, IntervalSeconds: 1,
		Source: domain.Source{Kind: domain.SourceDataset, Dataset: &source},
	}
}

// Ten channels replaying the same upload must not mean ten GridFS fetches and
// ten CSV parses. fetchSeries caches a parsed upload per dataset id for the
// span of one load, and this covers both callers that share it: the zone walk
// and the context-source loader.
func TestLoadSeriesFetchesASharedUploadOnce(t *testing.T) {
	csv := "Zeit;a;b\n" +
		"2026-01-05 00:00:00;1;10\n" +
		"2026-01-05 00:00:01;2;20\n" +
		"2026-01-05 00:00:02;4;40\n"
	store := &fakeRuntimeDatasets{
		meta:    repo.DatasetMeta{Id: "d1", Owner: "user-a", Name: "Lastgang", Timezone: "Europe/Berlin"},
		content: []byte(csv),
	}
	envId := "env-shared"
	env := testEnvironment(envId,
		datasetChannelWithColumn("ch-a", envId, "d1", "a"),
		datasetChannelWithColumn("ch-b", envId, "d1", "b"),
	)
	env.ContextSources = map[string]domain.Source{
		"weather": {Kind: domain.SourceDataset, IntervalSeconds: 1,
			Dataset: &domain.DatasetSource{Origin: domain.OriginFile, Ref: "d1", Column: "a",
				Resample: domain.ResampleHold, Anchor: domain.AnchorLoop}},
	}

	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), store, &fakePublisher{})
	result := rt.loadSeries(context.Background(), env)

	if got := store.contentCallsFor("d1"); got != 1 {
		t.Fatalf("two channels and a context source on the same upload have to fetch it once, got %d calls", got)
	}
	if got := result["ch-a"]; len(got) != 3 || got[0].Value != 1 {
		t.Fatalf("channel a did not get its own column out of the shared parse, got %v", got)
	}
	if got := result["ch-b"]; len(got) != 3 || got[0].Value != 10 {
		t.Fatalf("channel b did not get its own column out of the shared parse, got %v", got)
	}
	if got := result[contextSeriesId("weather")]; len(got) != 3 || got[0].Value != 1 {
		t.Fatalf("the context source did not get its column out of the shared parse, got %v", got)
	}
}

// Two channels on two different uploads still fetch one file each - the cache
// is keyed by dataset id, not shared across datasets.
func TestLoadSeriesFetchesTwoDatasetsEachOnce(t *testing.T) {
	csvA := "Zeit;wert\n2026-01-05 00:00:00;1\n2026-01-05 00:00:01;2\n"
	csvB := "Zeit;wert\n2026-01-05 00:00:00;10\n2026-01-05 00:00:01;20\n"
	store := &fakeRuntimeDatasets{
		meta:    repo.DatasetMeta{Id: "d1", Owner: "user-a", Name: "A", Timezone: "Europe/Berlin"},
		content: []byte(csvA),
	}
	store.add(repo.DatasetMeta{Id: "d2", Owner: "user-a", Name: "B", Timezone: "Europe/Berlin"}, []byte(csvB))

	envId := "env-two-datasets"
	env := testEnvironment(envId,
		datasetChannelWithColumn("ch-a", envId, "d1", ""),
		datasetChannelWithColumn("ch-b", envId, "d2", ""),
	)

	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), store, &fakePublisher{})
	result := rt.loadSeries(context.Background(), env)

	if got := store.contentCallsFor("d1"); got != 1 {
		t.Errorf("dataset d1 expected exactly one fetch, got %d", got)
	}
	if got := store.contentCallsFor("d2"); got != 1 {
		t.Errorf("dataset d2 expected exactly one fetch, got %d", got)
	}
	if got := result["ch-a"]; len(got) != 2 || got[0].Value != 1 {
		t.Errorf("channel a did not load dataset d1, got %v", got)
	}
	if got := result["ch-b"]; len(got) != 2 || got[0].Value != 10 {
		t.Errorf("channel b did not load dataset d2, got %v", got)
	}
}

// The series cache must not outlive the load it was built for: a reload after
// the dataset was replaced has to see the new content, not a parse from before
// the replace. Counting the fetches shows that the file is read again; the
// values published after the reload show that what is played is the new file,
// which is the property the count is only a proxy for.
func TestReloadRefetchesTheUpload(t *testing.T) {
	before := "Zeit;wert\n2026-01-05 00:00:00;1\n2026-01-05 00:00:01;2\n2026-01-05 00:00:02;4\n"
	after := "Zeit;wert\n2026-01-05 00:00:00;100\n2026-01-05 00:00:01;200\n2026-01-05 00:00:02;400\n"
	store := &fakeRuntimeDatasets{
		meta:    repo.DatasetMeta{Id: "d1", Owner: "user-a", Name: "Lastgang", Timezone: "Europe/Berlin"},
		content: []byte(before),
	}
	source := replaySource(domain.ResampleHold, domain.AnchorLoop)
	envId := "env-reload"
	env := testEnvironment(envId, datasetChannel(envId, source))

	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), store, publisher)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	if got := store.contentCallsFor("d1"); got != 1 {
		t.Fatalf("starting the environment expected exactly one fetch, got %d", got)
	}
	if !waitFor(5*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("the replay of the first upload never published")
	}

	store.replaceContent([]byte(after))
	rt.Reload(envId)

	if got := store.contentCallsFor("d1"); got != 2 {
		t.Fatalf("a reload must fetch the upload again rather than reuse the cache of the previous load, got %d calls", got)
	}

	//Reload waits for the runners of the old generation before starting the new
	//ones, so everything published from here on belongs to the new generation
	//and has to come out of the replaced file
	replayed := len(publisher.all())
	fresh := func() []float64 {
		result := []float64{}
		for _, event := range publisher.all()[replayed:] {
			if number, ok := event.value.(float64); ok {
				result = append(result, number)
			}
		}
		return result
	}
	if !waitFor(5*time.Second, func() bool { return len(fresh()) > 0 }) {
		t.Fatal("the reloaded environment never published")
	}
	for _, value := range fresh() {
		if value != 100 && value != 200 && value != 400 {
			t.Fatalf("the reload replayed the parse of the file as it was before it was replaced, got %v", value)
		}
	}
}
