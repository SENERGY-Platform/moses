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
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// A measurement of the compute side of a history run, skipped unless a
// document is given. It runs the engine over a window against a publisher that
// costs nothing, so what it measures is the loop and the executors alone.
//
//	MOSES_PROFILE_ENVIRONMENT=/path/environment.rendered.json \
//	MOSES_PROFILE_WEATHER=/path/wetter.csv MOSES_PROFILE_DAYS=7 \
//	go test ./lib/runtime/ -run TestProfileTheHistoryRunOfADocument -v -cpuprofile cpu.out

type discardingPublisher struct{}

func (discardingPublisher) PublishEvent(string, string, interface{}) error { return nil }

func (discardingPublisher) PublishEventAt(string, string, interface{}, time.Time) error {
	return nil
}

func (discardingPublisher) TimeShapeOf(string, string) (devices.TimeShape, error) {
	return devices.TimeShape{
		RootName:     "root",
		ValuePath:    []string{"value"},
		TimePath:     []string{"time"},
		TimeEncoding: devices.TimeAsUnixMilliseconds,
	}, nil
}

func TestProfileTheHistoryRunOfADocument(t *testing.T) {
	path := os.Getenv("MOSES_PROFILE_ENVIRONMENT")
	if path == "" {
		t.Skip("set MOSES_PROFILE_ENVIRONMENT to a rendered environment document")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var def domain.Environment
	if err := json.Unmarshal(content, &def); err != nil {
		t.Fatal(err)
	}
	if def.Id == "" {
		def.Id = "profile"
	}
	//a rendered document carries its platform refs only after the deploy; a
	//synthetic ref per asset and channel lets every channel take the publish path
	var refer func(zones []domain.Zone)
	refer = func(zones []domain.Zone) {
		for zi := range zones {
			refer(zones[zi].Zones)
			for ai := range zones[zi].Assets {
				asset := &zones[zi].Assets[ai]
				if asset.ExternalRef == "" {
					asset.ExternalRef = "urn:profile:device:" + asset.Id
				}
				for ci := range asset.Channels {
					if asset.Channels[ci].ExternalRef == "" {
						asset.Channels[ci].ExternalRef = "urn:profile:service:" + asset.Channels[ci].Id
					}
				}
			}
		}
	}
	refer(def.Zones)

	series := map[string][]dataset.Point{}
	if weather := os.Getenv("MOSES_PROFILE_WEATHER"); weather != "" {
		csv, err := os.ReadFile(weather)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := dataset.ParseCSV(csv, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		for key, source := range def.ContextSources {
			if source.Kind != domain.SourceDataset || source.Dataset == nil {
				continue
			}
			for _, column := range columns {
				if column.Name == source.Dataset.Column {
					series[contextSeriesId(key)] = column.Points
				}
			}
		}
	}

	days := 7
	if given := os.Getenv("MOSES_PROFILE_DAYS"); given != "" {
		days, err = strconv.Atoi(given)
		if err != nil {
			t.Fatal(err)
		}
	}
	from := time.Date(2025, 9, 8, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Duration(days) * 24 * time.Hour)

	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, discardingPublisher{})
	gen := newGeneration(def, series)
	env := &environment{id: def.Id, gen: gen, state: repo.RuntimeState{EnvironmentId: def.Id}}
	env.resetForHistory()
	env.seed(gen, from)

	started := time.Now()
	result, err := rt.runHistory(t.Context(), env, gen, from, to, keepTheWindow, nil)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)

	steps := int64(0)
	for _, channel := range result.Channels {
		steps += channel.Published + channel.Silent + channel.Failed
	}
	reasons := map[string]int{}
	for _, channel := range result.Channels {
		if !channel.Publishable {
			reasons[channel.Reason]++
		}
	}
	t.Logf("channels %d, publish steps %d, published %d, failed %d, unpublishable %v", len(result.Channels), steps, result.Published, result.Failed, reasons)
	t.Logf("window %d days in %v: %.0f publish steps/s, %.3f ms per step",
		days, elapsed.Round(time.Millisecond), float64(steps)/elapsed.Seconds(), float64(elapsed.Microseconds())/float64(steps)/1000)
	t.Logf("a year at this rate: %v", (elapsed * 365 / time.Duration(days)).Round(time.Minute))
}
