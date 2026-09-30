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

// The supervisor side of the SNRGY-4817 variant B spike: one worker process per
// environment, restarted with backoff, marked failed after repeated crashes.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
	"go.mongodb.org/mongo-driver/bson"
)

type SupervisorConfig struct {
	Binary   string
	ChildEnv []string // added to each worker's environment, e.g. JS_TIMEOUT=2s

	MemoryLimit uint64 // per worker, bytes; 0 means none
	Rlimit      string // "data" (default) or "as"

	// CrashDir holds one crash brake directory per environment; empty disables it.
	CrashDir string

	StallLimit  time.Duration // a script run in flight longer than this kills the worker
	BeatTimeout time.Duration // no heartbeat for this long kills the worker

	Backoff     time.Duration // first restart delay, doubled per crash in the window
	MaxBackoff  time.Duration
	MaxCrashes  int // crashes within CrashWindow that mark the environment failed
	CrashWindow time.Duration

	StartTimeout time.Duration

	// PublishConcurrency bounds the publishes the supervisor runs at once across
	// all workers, as PUBLISH_WORKERS did per run in process; 0 means 16.
	PublishConcurrency int

	Log     io.Writer // worker stdout and stderr lines, prefixed with the environment; nil discards
	OnEvent func(WorkerEvent)
}

func (this SupervisorConfig) withDefaults() SupervisorConfig {
	if this.StallLimit <= 0 {
		this.StallLimit = 10 * time.Second
	}
	if this.BeatTimeout <= 0 {
		this.BeatTimeout = 5 * time.Second
	}
	if this.Backoff <= 0 {
		this.Backoff = time.Second
	}
	if this.MaxBackoff <= 0 {
		this.MaxBackoff = time.Minute
	}
	if this.MaxCrashes <= 0 {
		this.MaxCrashes = 3
	}
	if this.CrashWindow <= 0 {
		this.CrashWindow = 10 * time.Minute
	}
	if this.PublishConcurrency <= 0 {
		this.PublishConcurrency = defaultPublishWorkers
	}
	if this.StartTimeout <= 0 {
		this.StartTimeout = time.Minute
	}
	return this
}

// WorkerEvent is one lifecycle step of an environment's worker.
type WorkerEvent struct {
	Env   string
	Kind  string // spawned, ready, crashed, restarting, failed, stopped
	At    time.Time
	Pid   int
	Cause string
}

type Supervisor struct {
	config    SupervisorConfig
	publisher eventPublisher

	mux  sync.Mutex
	envs map[string]*supervisedEnv

	// states and checkpoints stand in for the mongo store the supervisor owns.
	storeMux    sync.Mutex
	states      map[string][]byte
	checkpoints map[string][]byte

	// warm is the SNRGY-4664 first-produce lock, which has to be process wide in
	// the process that owns the connector - here, not in the workers.
	warmMux sync.RWMutex
	warm    map[string]bool

	readings     atomic.Int64
	publishSlots chan struct{}
}

func newSupervisor(config SupervisorConfig, publisher eventPublisher) *Supervisor {
	config = config.withDefaults()
	return &Supervisor{
		publishSlots: make(chan struct{}, config.PublishConcurrency),
		config:       config,
		publisher:    publisher,
		envs:         map[string]*supervisedEnv{},
		states:       map[string][]byte{},
		checkpoints:  map[string][]byte{},
		warm:         map[string]bool{},
	}
}

func (this *Supervisor) event(event WorkerEvent) {
	if this.config.OnEvent != nil {
		this.config.OnEvent(event)
	}
}

type supervisedEnv struct {
	sup *Supervisor
	id  string
	doc []byte

	mux          sync.Mutex
	proc         *workerProc
	crashes      []time.Time
	failed       bool
	cause        string
	channel      string // from the crash brake of the restarted worker
	stopped      bool
	restartTimer *time.Timer
}

// Start spawns the worker of one environment and waits until it runs.
func (this *Supervisor) Start(def domain.Environment) error {
	doc, err := bson.Marshal(def)
	if err != nil {
		return err
	}
	this.mux.Lock()
	if _, known := this.envs[def.Id]; known {
		this.mux.Unlock()
		return fmt.Errorf("environment %v is already supervised", def.Id)
	}
	env := &supervisedEnv{sup: this, id: def.Id, doc: doc}
	this.envs[def.Id] = env
	this.mux.Unlock()
	return env.launch()
}

func (this *Supervisor) env(id string) *supervisedEnv {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.envs[id]
}

// Status reports whether the environment's worker is up, and the cause when it failed.
func (this *Supervisor) Status(id string) (running bool, failed bool, cause string, channel string) {
	env := this.env(id)
	if env == nil {
		return false, false, "", ""
	}
	env.mux.Lock()
	defer env.mux.Unlock()
	return env.proc != nil, env.failed, env.cause, env.channel
}

// Pid is the process id of the environment's current worker, 0 when none runs.
func (this *Supervisor) Pid(id string) int {
	env := this.env(id)
	if env == nil {
		return 0
	}
	env.mux.Lock()
	defer env.mux.Unlock()
	if env.proc == nil {
		return 0
	}
	return env.proc.cmd.Process.Pid
}

func (this *supervisedEnv) current() (*workerProc, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if this.failed {
		return nil, fmt.Errorf("%w: the environment was marked failed: %v", repo.ErrNotRunning, this.cause)
	}
	if this.proc == nil {
		return nil, fmt.Errorf("%w: the worker is restarting", repo.ErrNotRunning)
	}
	return this.proc, nil
}

func (this *supervisedEnv) launch() error {
	proc, err := this.sup.spawn(this.id)
	if err != nil {
		this.crashed(nil, "unable to spawn the worker: "+err.Error())
		return err
	}
	this.mux.Lock()
	if this.stopped {
		this.mux.Unlock()
		proc.kill("stopped while spawning")
		return errors.New("stopped")
	}
	this.proc = proc
	this.mux.Unlock()
	proc.run(func(cause string) { this.crashed(proc, cause) })
	this.sup.event(WorkerEvent{Env: this.id, Kind: "spawned", At: time.Now(), Pid: proc.cmd.Process.Pid})

	answer, err := proc.call(&wireMsg{Kind: wireStart, Blob: this.doc}, this.sup.config.StartTimeout)
	if err != nil {
		proc.kill("the start failed: " + err.Error())
		return err
	}
	for _, decision := range answer.Decisions {
		if decision.Environment == this.id {
			this.mux.Lock()
			this.channel = decision.Channel
			this.mux.Unlock()
		}
	}
	this.sup.event(WorkerEvent{Env: this.id, Kind: "ready", At: time.Now(), Pid: proc.cmd.Process.Pid})
	return nil
}

// crashed books a death that was not asked for and decides between a restart
// with backoff and marking the environment failed.
func (this *supervisedEnv) crashed(proc *workerProc, cause string) {
	now := time.Now()
	config := this.sup.config
	this.mux.Lock()
	if proc != nil && this.proc != proc {
		this.mux.Unlock()
		return
	}
	this.proc = nil
	pid := 0
	if proc != nil {
		pid = proc.cmd.Process.Pid
	}
	if this.stopped {
		this.mux.Unlock()
		this.sup.event(WorkerEvent{Env: this.id, Kind: "stopped", At: now, Pid: pid, Cause: cause})
		return
	}
	this.crashes = crashesWithin(append(this.crashes, now), now, config.CrashWindow)
	this.cause = cause
	count := len(this.crashes)
	if count >= config.MaxCrashes {
		this.failed = true
		this.mux.Unlock()
		this.sup.event(WorkerEvent{Env: this.id, Kind: "crashed", At: now, Pid: pid, Cause: cause})
		this.sup.event(WorkerEvent{Env: this.id, Kind: "failed", At: now, Pid: pid, Cause: cause})
		return
	}
	delay := restartDelay(config.Backoff, config.MaxBackoff, count)
	this.restartTimer = time.AfterFunc(delay, func() {
		this.mux.Lock()
		skip := this.stopped || this.failed
		this.mux.Unlock()
		if !skip {
			_ = this.launch()
		}
	})
	this.mux.Unlock()
	this.sup.event(WorkerEvent{Env: this.id, Kind: "crashed", At: now, Pid: pid, Cause: cause})
	this.sup.event(WorkerEvent{Env: this.id, Kind: "restarting", At: now, Pid: pid, Cause: delay.String()})
}

// crashesWithin keeps the crashes not older than window, oldest first.
func crashesWithin(crashes []time.Time, now time.Time, window time.Duration) []time.Time {
	kept := crashes[:0]
	for _, at := range crashes {
		if now.Sub(at) <= window {
			kept = append(kept, at)
		}
	}
	return kept
}

// restartDelay doubles base per crash counted so far, capped at max; the shift
// is bounded so a long crash history cannot overflow into a negative delay.
func restartDelay(base time.Duration, max time.Duration, crashes int) time.Duration {
	delay := base
	for i := 1; i < crashes && delay < max; i++ {
		delay *= 2
	}
	if delay > max {
		delay = max
	}
	return delay
}

// Stop ends the worker of one environment cleanly: its runtime stops and flushes.
func (this *Supervisor) Stop(id string) {
	env := this.env(id)
	if env == nil {
		return
	}
	env.mux.Lock()
	env.stopped = true
	if env.restartTimer != nil {
		env.restartTimer.Stop()
	}
	proc := env.proc
	env.mux.Unlock()
	if proc == nil {
		return
	}
	_, _ = proc.call(&wireMsg{Kind: wireStop}, 30*time.Second)
	select {
	case <-proc.exited:
	case <-time.After(10 * time.Second):
		proc.kill("did not exit after stop")
		<-proc.exited
	}
}

// StopAll stops every worker in parallel.
func (this *Supervisor) StopAll() {
	this.mux.Lock()
	ids := make([]string, 0, len(this.envs))
	for id := range this.envs {
		ids = append(ids, id)
	}
	this.mux.Unlock()
	var wait sync.WaitGroup
	for _, id := range ids {
		wait.Add(1)
		go func() {
			defer wait.Done()
			this.Stop(id)
		}()
	}
	wait.Wait()
}

func (this *Supervisor) SetState(id string, change repo.StateChange) error {
	env := this.env(id)
	if env == nil {
		return repo.ErrNotRunning
	}
	proc, err := env.current()
	if err != nil {
		return err
	}
	blob, err := bson.Marshal(change)
	if err != nil {
		return err
	}
	_, err = proc.call(&wireMsg{Kind: wireSetState, Env: id, Blob: blob}, storeTimeout)
	return err
}

func (this *Supervisor) Snapshot(id string) (StateSnapshot, error) {
	env := this.env(id)
	if env == nil {
		return StateSnapshot{}, repo.ErrNotRunning
	}
	proc, err := env.current()
	if err != nil {
		return StateSnapshot{}, err
	}
	answer, err := proc.call(&wireMsg{Kind: wireSnapshot, Env: id}, storeTimeout)
	if err != nil {
		return StateSnapshot{}, err
	}
	var snapshot StateSnapshot
	if err := bson.Unmarshal(answer.Blob, &snapshot); err != nil {
		return StateSnapshot{}, err
	}
	snapshot.State.Context = plainMap(snapshot.State.Context)
	for zone, values := range snapshot.State.Zones {
		snapshot.State.Zones[zone] = plainMap(values)
	}
	for asset, values := range snapshot.State.Assets {
		snapshot.State.Assets[asset] = plainMap(values)
	}
	return snapshot, nil
}

// ProfileResult is one history run through a worker, measured on both sides.
type ProfileResult struct {
	wireProfileResult
	Wall      time.Duration // parent: from the request to the reply
	Spawn     time.Duration // parent: from exec to the first heartbeat-free reply of the worker
	SentBytes int64         // parent -> worker
	RecvBytes int64         // worker -> parent
	SentMsgs  int64
	RecvMsgs  int64
	Readings  int64
	Batches   int64 // reading frames the parent received
	ChildUser time.Duration
	ChildSys  time.Duration
	ChildHWM  int64 // KiB, VmHWM of the worker just before it stops
}

// RunProfile runs the engine over [from, to) of def in a fresh worker, like
// TestProfileTheHistoryRunOfADocument does in process.
func (this *Supervisor) RunProfile(def domain.Environment, from time.Time, to time.Time, checkpoints bool) (ProfileResult, error) {
	doc, err := bson.Marshal(def)
	if err != nil {
		return ProfileResult{}, err
	}
	spawned := time.Now()
	proc, err := this.spawn(def.Id)
	if err != nil {
		return ProfileResult{}, err
	}
	proc.run(func(string) {})
	//a snapshot of an environment the worker does not host answers at once, which
	//is the cheapest way to time the start of the process itself
	_, _ = proc.call(&wireMsg{Kind: wireSnapshot, Env: def.Id}, this.config.StartTimeout)
	result := ProfileResult{Spawn: time.Since(spawned)}
	flags := uint32(0)
	if checkpoints {
		flags |= wireProfileCheckpoints
	}
	started := time.Now()
	answer, err := proc.call(&wireMsg{Kind: wireProfile, Blob: doc, From: from.UnixNano(), To: to.UnixNano(), Flags: flags}, 24*time.Hour)
	result.Wall = time.Since(started)
	stats := proc.conn.stats
	result.SentBytes, result.RecvBytes = stats.SentBytes.Load(), stats.RecvBytes.Load()
	result.SentMsgs, result.RecvMsgs = stats.SentMsgs.Load(), stats.RecvMsgs.Load()
	result.Readings = proc.readings.Load()
	result.Batches = proc.readingFrames.Load()
	result.ChildHWM = procHWM(proc.cmd.Process.Pid)
	_, _ = proc.call(&wireMsg{Kind: wireStop}, 30*time.Second)
	<-proc.exited
	if usage, ok := proc.state.SysUsage().(*syscall.Rusage); ok {
		result.ChildUser = time.Duration(usage.Utime.Nano())
		result.ChildSys = time.Duration(usage.Stime.Nano())
	}
	if err != nil {
		return result, err
	}
	if answer.Profile != nil {
		result.wireProfileResult = *answer.Profile
	}
	return result, nil
}

// procHWM is VmHWM in KiB. Not the rusage maxrss: Linux carries the forking
// parent's high-water mark into the child's across the exec.
func procHWM(pid int) int64 {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(status), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "VmHWM:" {
			value, _ := strconv.ParseInt(fields[1], 10, 64)
			return value
		}
	}
	return 0
}

// workerProc is one incarnation of a worker process.
type workerProc struct {
	sup    *Supervisor
	env    string
	cmd    *exec.Cmd
	conn   *wireConn
	outbox *wireOutbox
	calls  wireCalls
	tail   *tailBuffer

	started    time.Time
	lastBeat   atomic.Int64 // nanos since started, on the monotonic clock
	inRun      atomic.Int64
	killReason atomic.Pointer[string]

	readings      atomic.Int64
	readingFrames atomic.Int64

	exited chan struct{}
	state  *os.ProcessState
}

func (this *Supervisor) spawn(env string) (*workerProc, error) {
	toChildR, toChildW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fromChildR, fromChildW, err := os.Pipe()
	if err != nil {
		toChildR.Close()
		toChildW.Close()
		return nil, err
	}
	cmd := exec.Command(this.config.Binary)
	cmd.Env = append(append(os.Environ(), WorkerModeEnv+"=1"), this.config.ChildEnv...)
	if this.config.MemoryLimit > 0 {
		cmd.Env = append(cmd.Env, WorkerMemoryLimitEnv+"="+strconv.FormatUint(this.config.MemoryLimit, 10))
		if this.config.Rlimit != "" {
			cmd.Env = append(cmd.Env, WorkerRlimitEnv+"="+this.config.Rlimit)
		}
	}
	if this.config.CrashDir != "" {
		//the directory name is derived from the id through its hex form, so no id can reach outside CrashDir
		cmd.Env = append(cmd.Env, WorkerCrashDirEnv+"="+filepath.Join(this.config.CrashDir, fmt.Sprintf("%x", env)))
	}
	cmd.ExtraFiles = []*os.File{toChildR, fromChildW}
	tail := newTailBuffer(64<<10, this.config.Log, env)
	cmd.Stdout = tail
	cmd.Stderr = tail
	cmd.SysProcAttr = workerProcAttr()
	if err := cmd.Start(); err != nil {
		toChildR.Close()
		toChildW.Close()
		fromChildR.Close()
		fromChildW.Close()
		return nil, err
	}
	//the worker's ends belong to the worker now; keeping them open here would hide its death as a missing EOF
	toChildR.Close()
	fromChildW.Close()
	proc := &workerProc{
		sup:     this,
		env:     env,
		cmd:     cmd,
		conn:    newWireConn(fromChildR, toChildW),
		tail:    tail,
		started: time.Now(),
		exited:  make(chan struct{}),
	}
	proc.outbox = newWireOutbox(proc.conn)
	return proc, nil
}

func (this *workerProc) run(onDeath func(cause string)) {
	go this.outbox.run(func(err error) { this.kill("the pipe to the worker broke: " + err.Error()) })
	go this.readLoop(onDeath)
	go this.watchdog()
}

func (this *workerProc) kill(reason string) {
	this.killReason.CompareAndSwap(nil, &reason)
	_ = this.cmd.Process.Kill()
}

func (this *workerProc) call(msg *wireMsg, timeout time.Duration) (*wireMsg, error) {
	id, reply, err := this.calls.open()
	if err != nil {
		return nil, err
	}
	msg.Id = id
	if !this.outbox.send(msg) {
		this.calls.forget(id)
		return nil, io.ErrClosedPipe
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case answer := <-reply:
		return answer, fromWireError(answer.Err)
	case <-timer.C:
		this.calls.forget(id)
		return nil, errors.New("the worker did not answer in time")
	}
}

func (this *workerProc) readLoop(onDeath func(cause string)) {
	for {
		msg, err := this.conn.read()
		if err != nil {
			break
		}
		switch msg.Kind {
		case wireReply:
			this.calls.deliver(msg)
		case wireBeatKind:
			this.lastBeat.Store(int64(time.Since(this.started)))
			if msg.Beat != nil {
				this.inRun.Store(msg.Beat.InRunNanos)
			}
		case wireReadings:
			this.readingFrames.Add(1)
			this.readings.Add(int64(len(msg.Readings)))
			go this.publishFrame(msg.Readings)
		case wireLoadState, wireSaveState, wireDeleteState, wireTimeShape, wireCheckpoint:
			go this.serveStore(msg)
		}
	}
	this.outbox.close()
	waitErr := this.cmd.Wait()
	this.state = this.cmd.ProcessState
	cause := this.describe(waitErr)
	this.calls.fail(errors.New("the worker died: " + cause))
	close(this.exited)
	onDeath(cause)
}

// describe names why the worker ended: the supervisor's own kill reason, else
// the first lines of a Go crash report, else the exit status.
func (this *workerProc) describe(waitErr error) string {
	status := "exited"
	if this.state != nil {
		status = this.state.String()
	} else if waitErr != nil {
		status = waitErr.Error()
	}
	if reason := this.killReason.Load(); reason != nil {
		return *reason + " (" + status + ")"
	}
	if report := this.tail.Report(); report != "" {
		return report + " (" + status + ")"
	}
	return status
}

func (this *workerProc) watchdog() {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	config := this.sup.config
	for {
		select {
		case <-this.exited:
			return
		case <-ticker.C:
			sinceBeat := time.Since(this.started) - time.Duration(this.lastBeat.Load())
			if sinceBeat > config.BeatTimeout {
				this.kill(fmt.Sprintf("no heartbeat for %v", sinceBeat.Round(time.Millisecond)))
				return
			}
			if inRun := time.Duration(this.inRun.Load()); inRun > config.StallLimit {
				this.kill(fmt.Sprintf("a script run was in flight for %v", inRun.Round(time.Millisecond)))
				return
			}
		}
	}
}

// publishFrame publishes the readings of one frame in parallel and answers them
// in one ack frame. A channel has at most one reading in flight, so the
// parallelism cannot reorder the readings of one channel.
func (this *workerProc) publishFrame(readings []wireReading) {
	acks := make([]wireAck, len(readings))
	var wait sync.WaitGroup
	for i, reading := range readings {
		wait.Add(1)
		go func() {
			defer wait.Done()
			this.sup.publishSlots <- struct{}{}
			err := this.sup.publishReading(reading)
			<-this.sup.publishSlots
			acks[i] = wireAck{Seq: reading.Seq, Err: toWireError(err)}
		}()
	}
	wait.Wait()
	this.outbox.send(&wireMsg{Kind: wireAcks, Acks: acks})
}

func (this *Supervisor) publishReading(reading wireReading) error {
	value, err := decodeReadingValue(reading)
	if err != nil {
		return err
	}
	this.readings.Add(1)
	send := func() error {
		if reading.Timed {
			return this.publisher.PublishEventAt(reading.Device, reading.Service, value, time.Unix(0, reading.At))
		}
		return this.publisher.PublishEvent(reading.Device, reading.Service, value)
	}
	topic := model.ServiceIdToTopic(reading.Service)
	this.warmMux.RLock()
	if this.warm[topic] {
		defer this.warmMux.RUnlock()
		return send()
	}
	this.warmMux.RUnlock()
	this.warmMux.Lock()
	defer this.warmMux.Unlock()
	err = send()
	if err == nil {
		this.warm[topic] = true
	}
	return err
}

func (this *workerProc) serveStore(msg *wireMsg) {
	answer := &wireMsg{Kind: wireReply, Id: msg.Id}
	sup := this.sup
	switch msg.Kind {
	case wireLoadState:
		sup.storeMux.Lock()
		answer.Blob = sup.states[msg.Env]
		sup.storeMux.Unlock()
	case wireSaveState:
		if msg.Env != this.env {
			//a worker writes only the environment it hosts
			answer.Err = toWireError(fmt.Errorf("the worker of %v may not write %v", this.env, msg.Env))
			break
		}
		sup.storeMux.Lock()
		sup.states[msg.Env] = msg.Blob
		sup.storeMux.Unlock()
	case wireDeleteState:
		sup.storeMux.Lock()
		delete(sup.states, this.env)
		sup.storeMux.Unlock()
	case wireCheckpoint:
		sup.storeMux.Lock()
		sup.checkpoints[this.env] = msg.Blob
		sup.storeMux.Unlock()
	case wireTimeShape:
		shape, err := sup.publisher.TimeShapeOf(msg.Device, msg.Service)
		answer.Shape = &shape
		answer.Err = toWireError(err)
	}
	this.outbox.send(answer)
}

// tailBuffer keeps the last bytes a worker wrote, remembers the first lines of a
// Go crash report, and forwards complete lines, prefixed with the environment, to log.
type tailBuffer struct {
	mux     sync.Mutex
	limit   int
	buf     []byte
	log     io.Writer
	prefix  string
	partial []byte
	report  []string
}

func newTailBuffer(limit int, log io.Writer, env string) *tailBuffer {
	return &tailBuffer{limit: limit, log: log, prefix: "[worker " + env + "] "}
}

func (this *tailBuffer) Write(p []byte) (int, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.buf = append(this.buf, p...)
	if len(this.buf) > this.limit {
		this.buf = append([]byte(nil), this.buf[len(this.buf)-this.limit:]...)
	}
	this.partial = append(this.partial, p...)
	for {
		index := bytes.IndexByte(this.partial, '\n')
		if index < 0 {
			break
		}
		line := string(this.partial[:index])
		this.partial = this.partial[index+1:]
		switch {
		case len(this.report) == 1 && strings.TrimSpace(line) != "":
			this.report = append(this.report, line)
		case len(this.report) == 0 && isCrashLine(line):
			this.report = append(this.report, line)
		}
		if this.log != nil {
			_, _ = io.WriteString(this.log, this.prefix+line+"\n")
		}
	}
	//a line longer than the tail is not kept whole for the report scan
	if len(this.partial) > this.limit {
		this.partial = this.partial[len(this.partial)-this.limit:]
	}
	return len(p), nil
}

func isCrashLine(line string) bool {
	return strings.HasPrefix(line, "fatal error:") || strings.HasPrefix(line, "panic:") || strings.HasPrefix(line, "runtime:")
}

// Report is the first line of a Go crash report and the one after it, or "".
func (this *tailBuffer) Report() string {
	this.mux.Lock()
	defer this.mux.Unlock()
	return strings.Join(this.report, " | ")
}

func (this *tailBuffer) String() string {
	this.mux.Lock()
	defer this.mux.Unlock()
	return string(this.buf)
}
