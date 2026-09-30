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

// Measurements of the SNRGY-4817 variant B spike. Everything that needs the
// worker binary is skipped unless MOSES_WORKER_BINARY names a built moses.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"go.mongodb.org/mongo-driver/bson"
)

// ---------------------------------------------------------------------------
// codec and policy, no binary needed
// ---------------------------------------------------------------------------

func TestReadingValuesSurviveTheWire(t *testing.T) {
	values := []interface{}{
		1.5, 0.0, -0.0, math.MaxFloat64, int64(7), int64(1) << 60, "text", "", true, nil,
		map[string]interface{}{},
		[]interface{}{},
		map[string]interface{}{"a": nil, "b": []interface{}{1.0, "x", map[string]interface{}{}}},
		map[string]interface{}{"m": map[string]interface{}{"l": []interface{}{[]interface{}{"deep"}}}},
	}
	for _, value := range values {
		reading := wireReading{}
		if err := encodeReadingValue(value, &reading); err != nil {
			t.Fatalf("%#v: %v", value, err)
		}
		back, err := decodeReadingValue(reading)
		if err != nil {
			t.Fatalf("%#v: %v", value, err)
		}
		want, _ := json.Marshal(value)
		got, _ := json.Marshal(back)
		if string(want) != string(got) {
			t.Errorf("%#v arrived as %#v: %s instead of %s", value, back, got, want)
		}
		if !reflect.DeepEqual(value, back) {
			t.Errorf("%#v arrived as %#v", value, back)
		}
	}
	//not JSON, so compared as numbers: the fast path must not swallow them
	for _, special := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		reading := wireReading{}
		if err := encodeReadingValue(special, &reading); err != nil {
			t.Fatal(err)
		}
		if reading.IsNum {
			t.Errorf("%v took the number fast path", special)
		}
		back, err := decodeReadingValue(reading)
		if err != nil {
			t.Fatal(err)
		}
		number, ok := back.(float64)
		if !ok || !(math.IsNaN(special) && math.IsNaN(number) || number == special) {
			t.Errorf("%v arrived as %#v", special, back)
		}
	}
}

// A state decoded from bson into typed maps holds bson.A below a map; plainMap
// has to reach it, or a state that went through the store differs from one that did not.
func TestStateMapsLeaveNoBsonTypes(t *testing.T) {
	change := repo.StateChange{Assets: map[string]map[string]interface{}{"a": {
		"m": map[string]interface{}{"l": []interface{}{1.0, map[string]interface{}{"x": []interface{}{"y"}}}},
		"e": map[string]interface{}{},
	}}}
	blob, err := bson.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	var back repo.StateChange
	if err := bson.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	back.Assets["a"] = plainMap(back.Assets["a"])
	if !reflect.DeepEqual(back.Assets["a"], change.Assets["a"]) {
		t.Fatalf("%#v", back.Assets["a"])
	}
}

func TestWireErrorsKeepTheirSentinels(t *testing.T) {
	for _, sentinel := range []error{repo.ErrNotRunning, ErrHistoryRunning, devices.ErrNoTimePath, devices.ErrUnusableTimeShape, ErrPublishAborted} {
		wrapped := fmt.Errorf("context: %w", sentinel)
		back := fromWireError(toWireError(wrapped))
		if !errors.Is(back, sentinel) {
			t.Errorf("%v lost its sentinel", wrapped)
		}
		if back.Error() != wrapped.Error() {
			t.Errorf("message changed: %q", back.Error())
		}
	}
	back := fromWireError(toWireError(errors.New("plain")))
	if back.Error() != "plain" || errors.Is(back, repo.ErrNotRunning) {
		t.Errorf("a plain error came back as %v", back)
	}
	if fromWireError(toWireError(nil)) != nil {
		t.Error("nil did not stay nil")
	}
}

func TestRestartDelayDoublesAndStopsAtTheCap(t *testing.T) {
	cases := map[int]time.Duration{0: time.Second, 1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 7: time.Minute, 1 << 20: time.Minute}
	for crashes, want := range cases {
		if got := restartDelay(time.Second, time.Minute, crashes); got != want {
			t.Errorf("%d crashes: %v, want %v", crashes, got, want)
		}
	}
}

func TestCrashesWithinKeepsTheBoundary(t *testing.T) {
	now := time.Now()
	window := time.Minute
	kept := crashesWithin([]time.Time{now.Add(-window - time.Nanosecond), now.Add(-window), now}, now, window)
	if len(kept) != 2 || !kept[0].Equal(now.Add(-window)) {
		t.Fatalf("kept %v", kept)
	}
}

func TestTailBufferKeepsTheFirstReportLinesAcrossWrites(t *testing.T) {
	tail := newTailBuffer(64, nil, "e")
	for _, chunk := range []string{"noise\nfatal er", "ror: stack overflow\n\n", "goroutine 7 [running]:\n", strings.Repeat("x", 500) + "\n", "fatal error: later\n"} {
		_, _ = tail.Write([]byte(chunk))
	}
	if got := tail.Report(); got != "fatal error: stack overflow | goroutine 7 [running]:" {
		t.Fatalf("report %q", got)
	}
	if len(tail.String()) > 64 {
		t.Fatalf("tail kept %d bytes", len(tail.String()))
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func workerBinary(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("MOSES_WORKER_BINARY")
	if binary == "" {
		t.Skip("set MOSES_WORKER_BINARY to a moses binary built from this tree")
	}
	return binary
}

func loadavg() string {
	content, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "?"
	}
	fields := strings.Fields(string(content))
	if len(fields) < 3 {
		return "?"
	}
	return strings.Join(fields[:3], " ")
}

func selfCPU() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// memoryOf reads VmRSS, VmHWM and Pss of a process in KiB.
func memoryOf(pid int) (rss int64, hwm int64, pss int64) {
	status, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "VmRSS:":
			rss = value
		case "VmHWM:":
			hwm = value
		}
	}
	rollup, _ := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	for _, line := range strings.Split(string(rollup), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "Pss:" {
			pss, _ = strconv.ParseInt(fields[1], 10, 64)
		}
	}
	return rss, hwm, pss
}

// profileDocument is the document of TestProfileTheHistoryRunOfADocument,
// prepared the same way: a synthetic ref per asset and channel.
func profileDocument(t *testing.T, id string) domain.Environment {
	t.Helper()
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
	def.Id = id
	var refer func(zones []domain.Zone)
	refer = func(zones []domain.Zone) {
		for zi := range zones {
			refer(zones[zi].Zones)
			for ai := range zones[zi].Assets {
				asset := &zones[zi].Assets[ai]
				if asset.ExternalRef == "" || id != "profile" {
					asset.ExternalRef = "urn:profile:" + id + ":device:" + asset.Id
				}
				for ci := range asset.Channels {
					if asset.Channels[ci].ExternalRef == "" || id != "profile" {
						asset.Channels[ci].ExternalRef = "urn:profile:" + id + ":service:" + asset.Channels[ci].Id
					}
				}
			}
		}
	}
	refer(def.Zones)
	return def
}

// capIntervals shortens every interval of a live document to at most limit
// seconds, so every script has prepared its vm within a short measurement.
func capIntervals(def *domain.Environment, limit int64) {
	capped := func(value int64) int64 {
		if value > limit {
			return limit
		}
		return value
	}
	for key, source := range def.ContextSources {
		source.IntervalSeconds = capped(source.IntervalSeconds)
		def.ContextSources[key] = source
	}
	var walk func(zones []domain.Zone)
	walk = func(zones []domain.Zone) {
		for zi := range zones {
			walk(zones[zi].Zones)
			for ai := range zones[zi].Assets {
				for ci := range zones[zi].Assets[ai].Channels {
					channel := &zones[zi].Assets[ai].Channels[ci]
					channel.IntervalSeconds = capped(channel.IntervalSeconds)
					channel.Source.IntervalSeconds = capped(channel.Source.IntervalSeconds)
				}
			}
		}
	}
	walk(def.Zones)
}

func firstAssetId(def domain.Environment) string {
	for _, zone := range def.Zones {
		if len(zone.Assets) > 0 {
			return zone.Assets[0].Id
		}
	}
	return ""
}

// livePublisher counts live readings per device and costs nothing else.
type livePublisher struct {
	mux    sync.Mutex
	counts map[string]int
	times  map[string][]time.Time
	values map[string][]interface{}
}

func newLivePublisher() *livePublisher {
	return &livePublisher{counts: map[string]int{}, times: map[string][]time.Time{}, values: map[string][]interface{}{}}
}

func (this *livePublisher) PublishEvent(device string, service string, value interface{}) error {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.counts[device]++
	this.times[device] = append(this.times[device], time.Now())
	if len(this.values[service]) < 16 {
		this.values[service] = append(this.values[service], value)
	}
	return nil
}

func (this *livePublisher) PublishEventAt(device string, service string, value interface{}, _ time.Time) error {
	return this.PublishEvent(device, service, value)
}

func (this *livePublisher) TimeShapeOf(string, string) (devices.TimeShape, error) {
	return devices.TimeShape{}, devices.ErrNoTimePath
}

func (this *livePublisher) total() int {
	this.mux.Lock()
	defer this.mux.Unlock()
	total := 0
	for _, count := range this.counts {
		total += count
	}
	return total
}

// gapsOf is the reading count and the largest gap between readings of device in [from, to].
func (this *livePublisher) gapsOf(device string, from time.Time, to time.Time) (int, time.Duration) {
	this.mux.Lock()
	defer this.mux.Unlock()
	count := 0
	last := from
	var largest time.Duration
	for _, at := range this.times[device] {
		if at.Before(from) || at.After(to) {
			continue
		}
		count++
		if gap := at.Sub(last); gap > largest {
			largest = gap
		}
		last = at
	}
	if gap := to.Sub(last); gap > largest {
		largest = gap
	}
	return count, largest
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(p*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func durationsSummary(values []time.Duration) string {
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return fmt.Sprintf("n=%d p50=%v p90=%v p99=%v max=%v", len(sorted),
		percentile(sorted, 0.50), percentile(sorted, 0.90), percentile(sorted, 0.99), percentile(sorted, 1))
}

func spikeSupervisor(t *testing.T, publisher eventPublisher, onEvent func(WorkerEvent)) *Supervisor {
	t.Helper()
	limit := uint64(512 << 20)
	if given := os.Getenv("MOSES_SPIKE_MEMORY_LIMIT"); given != "" {
		parsed, err := strconv.ParseUint(given, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		limit = parsed
	}
	var log *os.File
	if os.Getenv("MOSES_SPIKE_WORKER_LOG") != "" {
		log = os.Stderr
	}
	config := SupervisorConfig{
		Binary:      workerBinary(t),
		ChildEnv:    []string{"JS_TIMEOUT=2s", "STATE_FLUSH_INTERVAL=5s", "LOGGER_LEVEL=warn"},
		MemoryLimit: limit,
		Rlimit:      os.Getenv("MOSES_SPIKE_RLIMIT"),
		CrashDir:    t.TempDir(),
		OnEvent:     onEvent,
	}
	if log != nil {
		config.Log = log
	}
	sup := newSupervisor(config, publisher)
	t.Cleanup(sup.StopAll)
	return sup
}

// ---------------------------------------------------------------------------
// functional: a worker hosts an environment
// ---------------------------------------------------------------------------

func TestWorkerHostsAnEnvironment(t *testing.T) {
	publisher := newLivePublisher()
	sup := spikeSupervisor(t, publisher, nil)
	def := testEnvironment("env-hosted",
		scriptChannel("ch-count", domain.Sensor, 1, "urn:svc:count", `var n = moses.asset.state.get("n") + 1; moses.asset.state.set("n", n); moses.service.send(n);`),
		scriptChannel("ch-map", domain.Sensor, 1, "urn:svc:map", `moses.service.send({"a": 1, "e": {}, "l": []});`),
	)
	if err := sup.Start(def); err != nil {
		t.Fatal(err)
	}
	if !waitFor(10*time.Second, func() bool {
		publisher.mux.Lock()
		defer publisher.mux.Unlock()
		return len(publisher.values["urn:svc:count"]) >= 2 && len(publisher.values["urn:svc:map"]) >= 1
	}) {
		t.Fatalf("no readings arrived: %v", publisher.counts)
	}
	publisher.mux.Lock()
	mapped, _ := json.Marshal(publisher.values["urn:svc:map"][0])
	publisher.mux.Unlock()
	if string(mapped) != `{"a":1,"e":{},"l":[]}` {
		t.Errorf("the object reading arrived as %s", mapped)
	}
	if err := sup.SetState(def.Id, repo.StateChange{Assets: map[string]map[string]interface{}{testAssetId: {"probe": 42.0, "nested": map[string]interface{}{"k": "v", "list": []interface{}{1.0, "x"}}}}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := sup.Snapshot(def.Id)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State.Assets[testAssetId]["probe"] != 42.0 || !reflect.DeepEqual(snapshot.State.Assets[testAssetId]["nested"], map[string]interface{}{"k": "v", "list": []interface{}{1.0, "x"}}) {
		t.Errorf("the snapshot does not show the change: %#v", snapshot.State.Assets[testAssetId])
	}
	err = sup.SetState(def.Id, repo.StateChange{Assets: map[string]map[string]interface{}{"no-such-asset": {"x": 1.0}}})
	if err == nil || !strings.Contains(err.Error(), "no-such-asset") {
		t.Errorf("a change to an unknown asset was answered with %v", err)
	}
	if _, err := sup.Snapshot("not-hosted"); !errors.Is(err, repo.ErrNotRunning) {
		t.Errorf("an unknown environment was answered with %v", err)
	}
	sup.Stop(def.Id)
	sup.storeMux.Lock()
	stored := len(sup.states[def.Id])
	sup.storeMux.Unlock()
	if stored == 0 {
		t.Error("the final flush of the stopped worker did not reach the supervisor")
	}
}

// A worker whose script crashes the process is restarted with backoff, marked
// failed after MaxCrashes, and the neighbour keeps publishing throughout.
func TestAWorkerCrashIsRestartedAndThenMarkedFailed(t *testing.T) {
	publisher := newLivePublisher()
	var mux sync.Mutex
	var events []WorkerEvent
	sup := spikeSupervisor(t, publisher, func(event WorkerEvent) {
		mux.Lock()
		events = append(events, event)
		mux.Unlock()
	})
	healthy := testEnvironment("env-healthy", scriptChannel("ch-1", domain.Sensor, 1, "urn:svc:healthy", `moses.service.send(1);`))
	bad := testEnvironment("env-bad", scriptChannel("ch-crash", domain.Sensor, 1, "urn:svc:bad", `var o = {}; o.toString = String.prototype.trim; String(o);`))
	if err := sup.Start(healthy); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := sup.Start(bad); err != nil {
		t.Fatal(err)
	}
	if !waitFor(60*time.Second, func() bool { _, failed, _, _ := sup.Status(bad.Id); return failed }) {
		t.Fatal("the crashing environment was not marked failed")
	}
	ended := time.Now()
	_, _, cause, channel := sup.Status(bad.Id)
	if !strings.Contains(cause, "stack") {
		t.Errorf("the cause does not name the stack overflow: %q", cause)
	}
	if channel != "ch-crash" {
		t.Errorf("the restarted worker's brake attributed the crash to %q", channel)
	}
	mux.Lock()
	crashes := 0
	for _, event := range events {
		if event.Env == bad.Id && event.Kind == "crashed" {
			crashes++
		}
	}
	mux.Unlock()
	if crashes != 3 {
		t.Errorf("%d crashes before failing, want 3", crashes)
	}
	count, gap := publisher.gapsOf(deviceRefOf(healthy.Id), started.Add(time.Second), ended)
	if running, _, _, _ := sup.Status(healthy.Id); !running || gap > 3*time.Second {
		t.Errorf("the neighbour suffered: running %v, %d readings, largest gap %v", running, count, gap)
	}
	if err := sup.SetState(bad.Id, repo.StateChange{}); !errors.Is(err, repo.ErrNotRunning) {
		t.Errorf("a failed environment answered %v", err)
	}
}

// ---------------------------------------------------------------------------
// measurement: the history run of a document
// ---------------------------------------------------------------------------

func profileWindow(t *testing.T) (time.Time, time.Time, int) {
	days := 30
	if given := os.Getenv("MOSES_PROFILE_DAYS"); given != "" {
		parsed, err := strconv.Atoi(given)
		if err != nil {
			t.Fatal(err)
		}
		days = parsed
	}
	from := time.Date(2025, 9, 8, 0, 0, 0, 0, time.UTC)
	return from, from.Add(time.Duration(days) * 24 * time.Hour), days
}

// TestSpikeProfileInProcess is TestProfileTheHistoryRunOfADocument with the
// load and the CPU time of the run printed on one line, for the interleaved runs.
func TestSpikeProfileInProcess(t *testing.T) {
	def := profileDocument(t, "profile")
	from, to, days := profileWindow(t)
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, newFakeHistoryJobs(), discardingPublisher{})
	gen := newGeneration(def, loadedSeries(nil))
	env := &environment{id: def.Id, gen: gen, state: repo.RuntimeState{EnvironmentId: def.Id}}
	env.resetForHistory()
	env.seed(gen, from)
	loadBefore := loadavg()
	cpuBefore := selfCPU()
	started := time.Now()
	result, err := rt.runHistory(t.Context(), env, gen, from, to, keepTheWindow, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	cpu := selfCPU() - cpuBefore
	steps := int64(0)
	for _, channel := range result.Channels {
		steps += channel.Published + channel.Silent + channel.Failed
	}
	_, hwm, _ := memoryOf(os.Getpid())
	fmt.Printf("SPIKE profile mode=in days=%d steps=%d published=%d elapsed=%v ms_per_step=%.4f cpu=%v hwm_kib=%d load_before=%q load_after=%q\n",
		days, steps, result.Published, elapsed.Round(time.Millisecond), float64(elapsed.Nanoseconds())/float64(steps)/1e6, cpu.Round(time.Millisecond), hwm, loadBefore, loadavg())
}

func TestSpikeProfileWorker(t *testing.T) {
	def := profileDocument(t, "profile")
	from, to, days := profileWindow(t)
	//each: whatever is queued goes out at once; instant: held until the loop's next wait
	batch := envOr("MOSES_SPIKE_BATCH", "each")
	//the in-flight window of the worker's pool; the supervisor still publishes at most 16 at once
	window := envOr("MOSES_SPIKE_CHILD_PUBLISH_WORKERS", "256")
	sup := newSupervisor(SupervisorConfig{
		Binary:   workerBinary(t),
		ChildEnv: []string{"JS_TIMEOUT=5s", "STATE_FLUSH_INTERVAL=1h", "LOGGER_LEVEL=error", WorkerBatchEnv + "=" + batch, "PUBLISH_WORKERS=" + window},
	}, discardingPublisher{})
	loadBefore := loadavg()
	cpuBefore := selfCPU()
	result, err := sup.RunProfile(def, from, to, os.Getenv("MOSES_PROFILE_CHECKPOINTS") != "")
	if err != nil {
		t.Fatal(err)
	}
	parentCPU := selfCPU() - cpuBefore
	elapsed := time.Duration(result.ElapsedNanos)
	steps := float64(result.Steps)
	fmt.Printf("SPIKE profile mode=worker days=%d steps=%d published=%d elapsed=%v ms_per_step=%.4f wall=%v spawn=%v child_cpu=%v parent_cpu=%v child_hwm_kib=%d batch=%s publish_workers=%s readings=%d frames_in=%d avg_batch=%.1f msgs_per_step=%.3f bytes_per_step=%.1f recv_bytes=%d sent_bytes=%d checkpoints=%d checkpoint_kib=%d load_before=%q load_after=%q\n",
		days, result.Steps, result.Published, elapsed.Round(time.Millisecond), float64(elapsed.Nanoseconds())/steps/1e6,
		result.Wall.Round(time.Millisecond), result.Spawn.Round(time.Millisecond),
		(result.ChildUser + result.ChildSys).Round(time.Millisecond), parentCPU.Round(time.Millisecond), result.ChildHWM, batch, window,
		result.Readings, result.Batches, float64(result.Readings)/math.Max(1, float64(result.Batches)),
		float64(result.SentMsgs+result.RecvMsgs)/steps, float64(result.SentBytes+result.RecvBytes)/steps,
		result.RecvBytes, result.SentBytes, result.Checkpoints, result.CheckpointBytes/1024, loadBefore, loadavg())
}

// ---------------------------------------------------------------------------
// measurement: memory with N live environments
// ---------------------------------------------------------------------------

func TestSpikeMemory(t *testing.T) {
	mode := os.Getenv("MOSES_SPIKE_MEMORY_MODE")
	if mode == "" {
		t.Skip("set MOSES_SPIKE_MEMORY_MODE to in or worker")
	}
	count, _ := strconv.Atoi(envOr("MOSES_SPIKE_MEMORY_ENVS", "1"))
	wait, err := time.ParseDuration(envOr("MOSES_SPIKE_MEMORY_WAIT", "60s"))
	if err != nil {
		t.Fatal(err)
	}
	defs := make([]domain.Environment, count)
	for i := range defs {
		defs[i] = profileDocument(t, fmt.Sprintf("mw-%d", i+1))
		capIntervals(&defs[i], 5)
	}
	publisher := newLivePublisher()
	switch mode {
	case "in":
		cfg := testConfig(5 * time.Second)
		cfg.JsTimeout = 2 * time.Second
		rt := newRuntime(cfg, newFakeEnvironments(defs...), newFakeStates(), nil, newFakeHistoryJobs(), publisher)
		started := time.Now()
		if err := rt.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		startup := time.Since(started)
		defer rt.Stop()
		time.Sleep(wait)
		rss, hwm, pss := memoryOf(os.Getpid())
		fmt.Printf("SPIKE memory mode=in envs=%d wait=%v startup=%v readings=%d main_rss_kib=%d main_hwm_kib=%d main_pss_kib=%d load=%q\n",
			count, wait, startup.Round(time.Millisecond), publisher.total(), rss, hwm, pss, loadavg())
	case "worker":
		sup := spikeSupervisor(t, publisher, nil)
		pids := []int{}
		if count == 0 {
			//an idle worker: the process with no environment, the floor of every child
			proc, err := sup.spawn("idle")
			if err != nil {
				t.Fatal(err)
			}
			proc.run(func(string) {})
			defer proc.kill("done")
			pids = append(pids, proc.cmd.Process.Pid)
		}
		startups := []string{}
		for _, def := range defs {
			started := time.Now()
			if err := sup.Start(def); err != nil {
				t.Fatal(err)
			}
			startups = append(startups, time.Since(started).Round(time.Millisecond).String())
			pids = append(pids, sup.Pid(def.Id))
		}
		time.Sleep(wait)
		rss, hwm, pss := memoryOf(os.Getpid())
		line := fmt.Sprintf("SPIKE memory mode=worker envs=%d wait=%v startup=%s readings=%d main_rss_kib=%d main_hwm_kib=%d main_pss_kib=%d", count, wait, strings.Join(startups, "/"), publisher.total(), rss, hwm, pss)
		var childRss, childPss int64
		for i, pid := range pids {
			crss, chwm, cpss := memoryOf(pid)
			childRss += crss
			childPss += cpss
			line += fmt.Sprintf(" child%d_rss_kib=%d child%d_hwm_kib=%d child%d_pss_kib=%d", i+1, crss, i+1, chwm, i+1, cpss)
		}
		fmt.Printf("%s children_rss_kib=%d children_pss_kib=%d total_pss_kib=%d load=%q\n", line, childRss, childPss, childPss+pss, loadavg())
	default:
		t.Fatalf("unknown mode %q", mode)
	}
}

// ---------------------------------------------------------------------------
// measurement: latency of a state change and a snapshot
// ---------------------------------------------------------------------------

type spikeTarget interface {
	SetState(id string, change repo.StateChange) error
	Snapshot(id string) (StateSnapshot, error)
}

func TestSpikeLatency(t *testing.T) {
	mode := os.Getenv("MOSES_SPIKE_LATENCY_MODE")
	if mode == "" {
		t.Skip("set MOSES_SPIKE_LATENCY_MODE to in or worker")
	}
	rounds, _ := strconv.Atoi(envOr("MOSES_SPIKE_LATENCY_ROUNDS", "2000"))
	def := profileDocument(t, "mw-latency")
	capIntervals(&def, 5)
	small := testEnvironment("small-latency", scriptChannel("ch-1", domain.Sensor, 1, "urn:svc:small", `moses.service.send(1);`))
	publisher := newLivePublisher()
	var target spikeTarget
	switch mode {
	case "in":
		cfg := testConfig(5 * time.Second)
		cfg.JsTimeout = 2 * time.Second
		rt := newRuntime(cfg, newFakeEnvironments(def, small), newFakeStates(), nil, newFakeHistoryJobs(), publisher)
		if err := rt.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer rt.Stop()
		target = rt
	case "worker":
		sup := spikeSupervisor(t, publisher, nil)
		for _, each := range []domain.Environment{def, small} {
			if err := sup.Start(each); err != nil {
				t.Fatal(err)
			}
		}
		target = sup
	default:
		t.Fatalf("unknown mode %q", mode)
	}
	time.Sleep(10 * time.Second)
	for _, subject := range []struct {
		id    string
		asset string
	}{{def.Id, firstAssetId(def)}, {small.Id, testAssetId}} {
		var sets, snaps []time.Duration
		for i := 0; i < rounds; i++ {
			change := repo.StateChange{Assets: map[string]map[string]interface{}{subject.asset: {"spike_probe": float64(i)}}}
			started := time.Now()
			if err := target.SetState(subject.id, change); err != nil {
				t.Fatal(err)
			}
			sets = append(sets, time.Since(started))
			started = time.Now()
			snapshot, err := target.Snapshot(subject.id)
			if err != nil {
				t.Fatal(err)
			}
			snaps = append(snaps, time.Since(started))
			if got := snapshot.State.Assets[subject.asset]["spike_probe"]; got != float64(i) {
				t.Fatalf("the snapshot shows %v after setting %d", got, i)
			}
			time.Sleep(time.Millisecond)
		}
		snapshot, _ := target.Snapshot(subject.id)
		size, _ := json.Marshal(snapshot.State)
		fmt.Printf("SPIKE latency mode=%s env=%s state_json_bytes=%d set_state: %s | snapshot: %s | load=%q\n",
			mode, subject.id, len(size), durationsSummary(sets), durationsSummary(snaps), loadavg())
	}
}

// ---------------------------------------------------------------------------
// measurement: containment
// ---------------------------------------------------------------------------

func TestSpikeContainment(t *testing.T) {
	workerBinary(t)
	if os.Getenv("MOSES_SPIKE_CONTAINMENT") == "" {
		t.Skip("set MOSES_SPIKE_CONTAINMENT=1 (or a comma list of scenarios)")
	}
	hang := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	hang.Start()
	defer func() {
		hang.CloseClientConnections()
		hang.Close()
	}()
	scenarios := []struct {
		name    string
		code    string
		observe time.Duration
	}{
		{"tostring", `var o = {}; o.toString = String.prototype.trim; String(o);`, 60 * time.Second},
		{"yield", `function* leaf() { yield 1; } function* wrap(inner) { yield* inner; } var g = leaf(); for (var i = 0; i < 300000; i++) { g = wrap(g); } g.next();`, 60 * time.Second},
		{"proxy", `var p = {}; for (var i = 0; i < 450000; i++) { p = new Proxy(p, {}); } p.x = 1;`, 60 * time.Second},
		{"protochain", `var o = {}; for (var i = 0; i < 1300000; i++) { o = Object.create(o); } o.x = 1;`, 60 * time.Second},
		{"reviver", `JSON.parse('{"a":1,"b":2}', function (k, v) { if (k === "a") { var d = []; for (var i = 0; i < 900000; i++) { d = [d]; } this.b = d; } return v; });`, 60 * time.Second},
		{"recursion", `function f(n) { return f(n + 1) + 1; } f(0);`, 15 * time.Second},
		{"loop", `while (true) {}`, 15 * time.Second},
		{"membomb", `var s = "x"; for (var i = 0; i < 40; i++) { s = s + s; } moses.service.send(s.length);`, 60 * time.Second},
		{"regex", `var s = ""; for (var i = 0; i < 28; i++) { s += "a"; } s += "!"; while (true) { /^(?=a)(a+)+$/.test(s); }`, 15 * time.Second},
		{"httpget", `httpGet("` + hang.URL + `/never");`, 75 * time.Second},
	}
	only := map[string]bool{}
	if list := os.Getenv("MOSES_SPIKE_CONTAINMENT"); list != "1" {
		for _, name := range strings.Split(list, ",") {
			only[strings.TrimSpace(name)] = true
		}
	}
	for _, scenario := range scenarios {
		if len(only) > 0 && !only[scenario.name] {
			continue
		}
		t.Run(scenario.name, func(t *testing.T) {
			publisher := newLivePublisher()
			var mux sync.Mutex
			var events []WorkerEvent
			logs := &lineCollector{}
			sup := spikeSupervisor(t, publisher, func(event WorkerEvent) {
				mux.Lock()
				events = append(events, event)
				mux.Unlock()
			})
			sup.config.Log = logs
			healthy := testEnvironment("healthy-"+scenario.name, scriptChannel("ch-1", domain.Sensor, 1, "urn:svc:healthy", `moses.service.send(1);`))
			bad := testEnvironment("bad-"+scenario.name, scriptChannel("ch-bad", domain.Sensor, 1, "urn:svc:bad", scenario.code))
			if err := sup.Start(healthy); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			if err := sup.Start(bad); err != nil {
				t.Fatal(err)
			}
			waitFor(scenario.observe, func() bool { _, failed, _, _ := sup.Status(bad.Id); return failed })
			ended := time.Now()
			running, failed, cause, channel := sup.Status(bad.Id)
			count, gap := publisher.gapsOf(deviceRefOf(healthy.Id), started.Add(time.Second), ended)
			healthyRunning, _, _, _ := sup.Status(healthy.Id)
			mux.Lock()
			timeline := []string{}
			crashes := 0
			for _, event := range events {
				if event.Env != bad.Id || event.Kind == "spawned" {
					continue
				}
				if event.Kind == "crashed" {
					crashes++
				}
				timeline = append(timeline, fmt.Sprintf("%s@%.1fs", event.Kind, event.At.Sub(started).Seconds()))
			}
			mux.Unlock()
			fmt.Printf("SPIKE containment scenario=%s observed=%.1fs crashes=%d failed=%v running=%v channel=%q cause=%q script_error=%q healthy_running=%v healthy_readings=%d healthy_max_gap=%v timeline=%s load=%q\n",
				scenario.name, ended.Sub(started).Seconds(), crashes, failed, running, channel, cause, logs.first(bad.Id, "script failed"), healthyRunning, count, gap.Round(time.Millisecond), strings.Join(timeline, ","), loadavg())
		})
	}
}

// lineCollector keeps the worker log lines for the report.
type lineCollector struct {
	mux   sync.Mutex
	lines []string
}

func (this *lineCollector) Write(p []byte) (int, error) {
	this.mux.Lock()
	this.lines = append(this.lines, string(p))
	this.mux.Unlock()
	return len(p), nil
}

func (this *lineCollector) first(env string, needle string) string {
	this.mux.Lock()
	defer this.mux.Unlock()
	for _, line := range this.lines {
		if strings.Contains(line, "[worker "+env+"]") && strings.Contains(line, needle) {
			if index := strings.Index(line, "error="); index >= 0 {
				line = line[index:]
			}
			if len(line) > 160 {
				line = line[:160]
			}
			return strings.TrimSpace(line)
		}
	}
	return ""
}
