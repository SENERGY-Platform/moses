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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/crashbrake"
	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"go.mongodb.org/mongo-driver/bson"
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

// recordingPublisher keeps every reading per service for an engine comparison
// (MOSES_PROFILE_DIGEST=file), spike only.
type recordingPublisher struct {
	discardingPublisher
	mux      sync.Mutex
	readings map[string][]string
}

func (this *recordingPublisher) PublishEventAt(device string, service string, value interface{}, at time.Time) error {
	encoded, _ := json.Marshal(value)
	if number, ok := value.(float64); ok {
		encoded = []byte(strconv.FormatFloat(number, 'g', -1, 64))
	}
	this.mux.Lock()
	this.readings[service] = append(this.readings[service], strconv.FormatInt(at.UnixMilli(), 10)+"|"+string(encoded))
	this.mux.Unlock()
	return nil
}

func (this *recordingPublisher) PublishEvent(device string, service string, value interface{}) error {
	return this.PublishEventAt(device, service, value, time.Time{})
}

// digest writes count, numeric sum and a hash of the time-ordered readings per service.
func (this *recordingPublisher) digest(path string) error {
	out := map[string]map[string]interface{}{}
	for service, readings := range this.readings {
		sort.Strings(readings)
		sum := 0.0
		hash := sha256.New()
		for _, reading := range readings {
			hash.Write([]byte(reading))
			hash.Write([]byte{10})
			if number, err := strconv.ParseFloat(reading[strings.IndexByte(reading, '|')+1:], 64); err == nil {
				sum += number
			}
		}
		out[service] = map[string]interface{}{"count": len(readings), "sum": sum, "sha256": hex.EncodeToString(hash.Sum(nil))[:16]}
	}
	if raw := os.Getenv("MOSES_PROFILE_RAW"); raw != "" {
		readings := append([]string(nil), this.readings[raw]...)
		sort.Strings(readings)
		if err := os.WriteFile(path+".raw", []byte(strings.Join(readings, "\n")), 0o644); err != nil {
			return err
		}
	}
	encoded, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o644)
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

	var publisher eventPublisher = discardingPublisher{}
	recorder := &recordingPublisher{readings: map[string][]string{}}
	digestPath := os.Getenv("MOSES_PROFILE_DIGEST")
	if digestPath != "" {
		publisher = recorder
	}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, newFakeHistoryJobs(), publisher)
	//MOSES_PROFILE_BRAKE=1 measures the per-run crash-brake cost (goID and the slot)
	if os.Getenv("MOSES_PROFILE_BRAKE") != "" {
		brake, _, brakeErr := crashbrake.Open(t.TempDir())
		if brakeErr != nil {
			t.Fatal(brakeErr)
		}
		defer brake.Close()
		rt.brake = brake
	}
	gen := newGeneration(def, loadedSeries(series))
	env := &environment{id: def.Id, gen: gen, state: repo.RuntimeState{EnvironmentId: def.Id}}
	env.resetForHistory()
	env.seed(gen, from)

	//MOSES_PROFILE_CHECKPOINTS=1 measures the chunk boundaries too: the drain, the
	//snapshot and the size of the document a store would receive
	checkpoints := 0
	checkpointBytes := 0
	var checkpoint historyCheckpointFunc
	if os.Getenv("MOSES_PROFILE_CHECKPOINTS") != "" {
		checkpoint = func(progress repo.HistoryJobProgress) error {
			checkpoints++
			encoded, err := bson.Marshal(progress.Checkpoint)
			if err != nil {
				return err
			}
			checkpointBytes += len(encoded)
			return nil
		}
	}
	started := time.Now()
	result, err := rt.runHistory(t.Context(), env, gen, from, to, keepTheWindow, nil, nil, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	instances, pages := env.scripts.qjs.pages()
	t.Logf("memory after the run: heap in use %d MB, sys %d MB, qjs instances kept %d holding %d MB of linear memory",
		memory.HeapInuse>>20, memory.Sys>>20, instances, pages>>20)
	if digestPath != "" {
		//the final state as well: script channels mostly write state that formulas publish
		record := func(scope string, values map[string]interface{}) {
			for key, value := range values {
				recorder.readings["state:"+scope+":"+key] = []string{"0|" + fmt.Sprintf("%#v", value)}
			}
		}
		record("context", env.state.Context)
		for id, values := range env.state.Zones {
			record("zone:"+id, values)
		}
		for id, values := range env.state.Assets {
			record("asset:"+id, values)
		}
		if err := recorder.digest(digestPath); err != nil {
			t.Fatal(err)
		}
	}

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
	if engineQJS {
		t.Logf("qjs: %d runs, %d host calls (%.1f per run), %d instances prepared", qjsCounters.runs.Load(), qjsCounters.hostCalls.Load(), float64(qjsCounters.hostCalls.Load())/float64(max(qjsCounters.runs.Load(), 1)), qjsCounters.prepares.Load())
	}
	if checkpoints > 0 {
		t.Logf("checkpoints %d, %d KB each", checkpoints, checkpointBytes/checkpoints/1024)
	}
}
