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

// The point of a context source, end to end: the context moves on its own, and
// everything that reads the context sees it move. The formula channel is the
// reader here - it publishes context.outdoor + 0, so the published values ARE
// the context values.
func TestAContextSourceDrivesWhatFormulasRead(t *testing.T) {
	env := testEnvironment("env-ctxsrc", formulaChannel("env-ctxsrc", "outdoor + 0", map[string]string{"outdoor": "context.outdoor"}))
	env.Seed = 7
	env.ContextSources = map[string]domain.Source{
		"outdoor": {Kind: domain.SourceProfile, IntervalSeconds: 1,
			Profile: &domain.ProfileSource{Base: 10, SpreadPercent: 50}},
	}
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	moved := func() bool {
		values := map[float64]bool{}
		for _, event := range publisher.all() {
			if value, ok := event.value.(float64); ok && value != 0 {
				values[value] = true
			}
		}
		return len(values) >= 2
	}
	if !waitFor(8*time.Second, moved) {
		t.Fatalf("the formula never saw a moving context, events: %v", publisher.all())
	}
}

func TestADatasetContextSourceReplaysIntoTheContext(t *testing.T) {
	now := time.Now().Unix()
	store := &fakeRuntimeDatasets{
		meta:    repo.DatasetMeta{Id: "d-weather", Owner: "o", Name: "Wetter", Timezone: "Europe/Berlin"},
		content: []byte("t,temp\n1,1\n2,2\n"), //content is irrelevant, the fetcher below overrides
	}
	env := testEnvironment("env-ctxds", formulaChannel("env-ctxds", "temp", map[string]string{"temp": "context.outdoor"}))
	env.ContextSources = map[string]domain.Source{
		"outdoor": {Kind: domain.SourceDataset, IntervalSeconds: 1,
			Dataset: &domain.DatasetSource{Origin: domain.OriginFile, Ref: "d-weather",
				Resample: domain.ResampleHold, Anchor: domain.AnchorLoop}},
	}
	publisher := &fakePublisher{}
	states := newFakeStates()
	rt := newRuntime(testConfig(50*time.Millisecond), newFakeEnvironments(env), states, store, publisher)
	//the parsed content: two points, values 11 and 22
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store.content = []byte("t;wert\n" + timeRow(now-2, "11") + timeRow(now-1, "22"))
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	sawDatasetValue := func() bool {
		for _, event := range publisher.all() {
			if event.value == 11.0 || event.value == 22.0 {
				return true
			}
		}
		return false
	}
	if !waitFor(8*time.Second, sawDatasetValue) {
		t.Fatalf("the context never carried a replayed value, events: %v", publisher.all())
	}
	//the anchor is persisted under the collision-safe id
	anchored := func() bool {
		for _, saved := range states.savedFor("env-ctxds") {
			if _, ok := saved.state.Anchors["context:outdoor"]; ok {
				return true
			}
		}
		return false
	}
	if !waitFor(4*time.Second, anchored) {
		t.Error("the replay anchor of the context source was never flushed")
	}
}

func timeRow(unix int64, value string) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02T15:04:05Z") + ";" + value + "\n"
}
