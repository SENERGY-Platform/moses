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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// a quarter-hour series: 100, 200, 400 at t=0, 900, 1800
var replayPoints = []dataset.Point{{Unix: 0, Value: 100}, {Unix: 900, Value: 200}, {Unix: 1800, Value: 400}}

func replaySource(resample domain.ResampleMode, anchor domain.AnchorMode) domain.DatasetSource {
	return domain.DatasetSource{Origin: domain.OriginFile, Ref: "d1", Resample: resample, Anchor: anchor}
}

func TestReplayResampling(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   domain.ResampleMode
		second int64
		want   float64
	}{
		{"hold keeps the last sample", domain.ResampleHold, 1000, 200},
		{"hold at an exact sample", domain.ResampleHold, 900, 200},
		{"linear interpolates", domain.ResampleLinear, 450, 150},
		{"linear between the later points", domain.ResampleLinear, 1350, 300},
		//a 30s tick gets its share of the 900s slot: 200 * 30/900
		{"distribute spreads the slot quantity", domain.ResampleDistribute, 1000, 200.0 * 30 / 900},
	} {
		t.Run(tc.name, func(t *testing.T) {
			//anchor 0, now = tc.second: the loop plays the series 1:1
			got, playable := replayValue(replaySource(tc.mode, domain.AnchorLoop), replayPoints, 0, time.Unix(tc.second, 0), 30)
			if !playable {
				t.Fatal("expected a value")
			}
			if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("expected %v, got %v", tc.want, got)
			}
		})
	}
}

func TestReplayLoopsAndOriginalStaysSilentOutsideItsRange(t *testing.T) {
	//span is 1800s; at elapsed 1800+450 the loop is back at 450
	looped, playable := replayValue(replaySource(domain.ResampleLinear, domain.AnchorLoop), replayPoints, 0, time.Unix(2250, 0), 30)
	if !playable || looped != 150 {
		t.Errorf("expected the second loop to replay 150, got %v (playable=%v)", looped, playable)
	}
	//original anchored: before and after the data there is nothing to say
	if _, playable = replayValue(replaySource(domain.ResampleHold, domain.AnchorOriginal), replayPoints, 0, time.Unix(5000, 0), 30); playable {
		t.Error("original anchor outside the range has to stay silent")
	}
	//the loop never reaches the last point (it wraps first); the original
	//anchor does, exactly at the end of the data
	atEnd, playable := replayValue(replaySource(domain.ResampleLinear, domain.AnchorOriginal), replayPoints, 0, time.Unix(1800, 0), 30)
	if !playable || atEnd != 400 {
		t.Errorf("expected the last point at the end of the range, got %v (playable=%v)", atEnd, playable)
	}
}

// A meter must not jump back to the first value when the loop wraps: every
// completed loop adds the full sweep of the series.
func TestReplayCumulativeKeepsCountingAcrossTheLoop(t *testing.T) {
	source := replaySource(domain.ResampleHold, domain.AnchorLoop)
	source.Cumulative = true
	endOfFirst, _ := replayValue(source, replayPoints, 0, time.Unix(1799, 0), 30)
	startOfSecond, _ := replayValue(source, replayPoints, 0, time.Unix(1800, 0), 30)
	if startOfSecond < endOfFirst {
		t.Errorf("the reading fell from %v to %v at the loop boundary", endOfFirst, startOfSecond)
	}
	//second loop start: first value 100 plus one full sweep (400-100)
	if startOfSecond != 400 {
		t.Errorf("expected 400 at the start of the second loop, got %v", startOfSecond)
	}
}

func TestReplayScales(t *testing.T) {
	source := replaySource(domain.ResampleHold, domain.AnchorLoop)
	source.Scale = 2.5
	got, _ := replayValue(source, replayPoints, 0, time.Unix(0, 0), 30)
	if got != 250 {
		t.Errorf("expected 250, got %v", got)
	}
}

// --- end to end over the runtime, with the store faked ---

// fakeRuntimeDatasets fakes repo.Datasets for one primary dataset (meta,
// content, set directly by a test) plus any number of others added through
// add(). contentCalls counts Content() calls per dataset id, which is how the
// series-cache tests pin down that a shared upload is fetched at most once
// per load.
type fakeRuntimeDatasets struct {
	meta    repo.DatasetMeta
	content []byte

	mux          sync.Mutex
	extra        map[string]fakeDataset
	contentCalls map[string]int
	// contentBudget is the context of the last Content call, which is how the
	// context tests show that a gridfs read after a platform fetch still runs
	// on a live budget rather than on one the fetch used up.
	contentBudget ctxBudget
}

type fakeDataset struct {
	meta    repo.DatasetMeta
	content []byte
}

func (this *fakeRuntimeDatasets) add(meta repo.DatasetMeta, content []byte) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if this.extra == nil {
		this.extra = map[string]fakeDataset{}
	}
	this.extra[meta.Id] = fakeDataset{meta: meta, content: content}
}

// replaceContent swaps the content of the primary dataset while the runtime is
// running. That is how a reload is shown to serve the file as it is now rather
// than the parse the previous load left in the cache.
func (this *fakeRuntimeDatasets) replaceContent(content []byte) {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.content = content
}

// lookup reads the primary dataset under the mutex too, because replaceContent
// writes it while the runtime is running.
func (this *fakeRuntimeDatasets) lookup(id string) (fakeDataset, bool) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if id == this.meta.Id {
		return fakeDataset{meta: this.meta, content: this.content}, true
	}
	d, ok := this.extra[id]
	return d, ok
}

func (this *fakeRuntimeDatasets) Create(ctx context.Context, meta repo.DatasetMeta, raw []byte) error {
	return nil
}
func (this *fakeRuntimeDatasets) Get(ctx context.Context, id string) (repo.DatasetMeta, error) {
	d, ok := this.lookup(id)
	if !ok {
		return repo.DatasetMeta{}, repo.ErrNotFound
	}
	return d.meta, nil
}
func (this *fakeRuntimeDatasets) ListByOwner(ctx context.Context, owner string) ([]repo.DatasetMeta, error) {
	return nil, nil
}
func (this *fakeRuntimeDatasets) All(ctx context.Context) ([]repo.DatasetMeta, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	return []repo.DatasetMeta{this.meta}, nil
}

func (this *fakeRuntimeDatasets) Content(ctx context.Context, id string) ([]byte, error) {
	this.mux.Lock()
	if this.contentCalls == nil {
		this.contentCalls = map[string]int{}
	}
	this.contentCalls[id]++
	this.contentBudget = budgetOf(ctx)
	this.mux.Unlock()
	//gridfs would fail on a spent context, and so does the fake: a channel
	//silently dropped because the context died earlier in the same load is
	//exactly what this has to be able to show
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d, ok := this.lookup(id)
	if !ok {
		return nil, repo.ErrNotFound
	}
	return d.content, nil
}

// contentCallsFor is how the series-cache tests read back the counter.
func (this *fakeRuntimeDatasets) contentCallsFor(id string) int {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.contentCalls[id]
}

func (this *fakeRuntimeDatasets) lastContentBudget() ctxBudget {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.contentBudget
}

func (this *fakeRuntimeDatasets) Delete(ctx context.Context, id string) error { return nil }

func datasetChannel(envId string, source domain.DatasetSource) domain.Channel {
	return domain.Channel{
		Id: "ch-1", Name: "replay", Direction: domain.Sensor, ExternalRef: serviceRefOf(envId), IntervalSeconds: 1,
		Source: domain.Source{Kind: domain.SourceDataset, Dataset: &source},
	}
}

func TestADatasetChannelReplaysItsUpload(t *testing.T) {
	//two columns, german dialect; the channel picks the second by name
	csv := "Zeit;strom;gas\n2026-01-05 00:00;1,0;10\n2026-01-05 00:00:01;2,0;20\n2026-01-05 00:00:02;4,0;40\n"
	store := &fakeRuntimeDatasets{
		meta:    repo.DatasetMeta{Id: "d1", Owner: "user-a", Name: "Lastgang", Timezone: "Europe/Berlin"},
		content: []byte(csv),
	}
	source := replaySource(domain.ResampleHold, domain.AnchorLoop)
	source.Column = "gas"
	env := testEnvironment("env-replay", datasetChannel("env-replay", source))

	publisher := &fakePublisher{}
	states := newFakeStates()
	rt := newRuntime(testConfig(50*time.Millisecond), newFakeEnvironments(env), states, store, publisher)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	if !waitFor(5*time.Second, func() bool { return publisher.count() >= 3 }) {
		t.Fatalf("the replay did not publish, count %d", publisher.count())
	}
	seen := map[float64]bool{}
	for _, value := range publisher.forDevice(deviceRefOf("env-replay")) {
		got, ok := value.(float64)
		if !ok {
			t.Fatalf("expected a number, got %T", value)
		}
		if got != 10 && got != 20 && got != 40 {
			t.Fatalf("expected only values of the gas column, got %v", got)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Errorf("a looping 3-point series on a 1s tick has to show different values, got %v", seen)
	}
	//the loop anchor is persisted, so a restart resumes instead of starting over
	flushed := func() bool {
		for _, saved := range states.savedFor("env-replay") {
			if len(saved.state.Anchors) > 0 {
				return true
			}
		}
		return false
	}
	if !waitFor(4*time.Second, flushed) {
		t.Error("the replay anchor was never flushed")
	}
}

func TestAChannelWhoseDatasetIsGoneDoesNotStopTheEnvironment(t *testing.T) {
	source := replaySource(domain.ResampleHold, domain.AnchorLoop)
	source.Ref = "missing"
	env := testEnvironment("env-gone",
		datasetChannel("env-gone", source),
		scriptChannel("ch-2", domain.Sensor, 1, serviceRefOf("env-gone")+"-b", `moses.service.send("alive");`))

	publisher := &fakePublisher{}
	store := &fakeRuntimeDatasets{meta: repo.DatasetMeta{Id: "other"}}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), store, publisher)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("the script channel has to keep running when a sibling's dataset is gone")
	}
	for _, event := range publisher.all() {
		if fmt.Sprint(event.value) != "alive" {
			t.Errorf("the dataset channel must not publish anything, got %v", event.value)
		}
	}
}
