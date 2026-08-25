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

type fakeRuntimeDatasets struct {
	meta    repo.DatasetMeta
	content []byte
}

func (this *fakeRuntimeDatasets) Create(ctx context.Context, meta repo.DatasetMeta, raw []byte) error {
	return nil
}
func (this *fakeRuntimeDatasets) Get(ctx context.Context, id string) (repo.DatasetMeta, error) {
	if id != this.meta.Id {
		return repo.DatasetMeta{}, repo.ErrNotFound
	}
	return this.meta, nil
}
func (this *fakeRuntimeDatasets) ListByOwner(ctx context.Context, owner string) ([]repo.DatasetMeta, error) {
	return nil, nil
}
func (this *fakeRuntimeDatasets) All(ctx context.Context) ([]repo.DatasetMeta, error) {
	return []repo.DatasetMeta{this.meta}, nil
}

func (this *fakeRuntimeDatasets) Content(ctx context.Context, id string) ([]byte, error) {
	if id != this.meta.Id {
		return nil, repo.ErrNotFound
	}
	return this.content, nil
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
