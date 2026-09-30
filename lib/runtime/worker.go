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

// The worker side of the SNRGY-4817 variant B spike: one process hosts one
// environment's runtime, and only the boundaries cross the pipe.

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/crashbrake"
	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/jsguard"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/util"
	"go.mongodb.org/mongo-driver/bson"
)

// WorkerModeEnv selects the worker mode of the binary; main checks it first.
const WorkerModeEnv = "MOSES_WORKER"

// scriptRunWatch is set by the worker before any script runs; in process it
// stays nil and costs one nil check per run.
var scriptRunWatch *runWatch

type runWatch struct {
	mux      sync.Mutex
	next     uint64
	inFlight map[uint64]time.Time
	runs     atomic.Int64
}

func newRunWatch() *runWatch { return &runWatch{inFlight: map[uint64]time.Time{}} }

func (this *runWatch) enter() func() {
	started := time.Now()
	this.mux.Lock()
	this.next++
	id := this.next
	this.inFlight[id] = started
	this.mux.Unlock()
	return func() {
		this.mux.Lock()
		delete(this.inFlight, id)
		this.mux.Unlock()
		this.runs.Add(1)
	}
}

// oldest is the age of the longest script run still in flight, on the monotonic clock.
func (this *runWatch) oldest() time.Duration {
	this.mux.Lock()
	defer this.mux.Unlock()
	var result time.Duration
	for _, started := range this.inFlight {
		if age := time.Since(started); age > result {
			result = age
		}
	}
	return result
}

type workerChild struct {
	conn   *wireConn
	outbox *wireOutbox
	calls  wireCalls
	config config.Config

	// brake is the crash brake of this worker alone, in a directory the
	// supervisor keeps per environment; decisions is what it read at boot.
	brake     *crashbrake.Brake
	decisions []crashbrake.Decision

	batchInstants bool

	mux  sync.Mutex
	rt   *Runtime
	envs *workerEnvironments
}

// WorkerBatchEnv "instant" makes a history run send its readings once per
// instant instead of each as soon as a pool worker has it.
const WorkerBatchEnv = "MOSES_WORKER_BATCH"

// WorkerCrashDirEnv names the per-environment crash brake directory of a worker.
const WorkerCrashDirEnv = "MOSES_WORKER_CRASH_DIR"

// RunWorker is the worker mode: the protocol runs on fd 3 (in) and fd 4 (out),
// stdout and stderr stay free for logs and a crash report.
func RunWorker() int {
	jsguard.LimitStack()
	util.InitLogger(envOr("LOGGER_HANDLER", "text"), envOr("LOGGER_LEVEL", "warn"))
	if err := applyWorkerLimits(); err != nil {
		util.Logger.Error("unable to apply the worker limits", attributes.ErrorKey, err)
		return 2
	}
	child := &workerChild{
		conn:   newWireConn(os.NewFile(3, "worker-in"), os.NewFile(4, "worker-out")),
		config: workerConfigFromEnv(),
	}
	child.outbox = newWireOutbox(child.conn)
	child.batchInstants = os.Getenv(WorkerBatchEnv) == "instant"
	publishPoolWaitHook = child.outbox.flushNow
	if dir := os.Getenv(WorkerCrashDirEnv); dir != "" {
		brake, decisions, err := crashbrake.Open(dir)
		if err != nil {
			util.Logger.Error("unable to open the worker crash brake", attributes.ErrorKey, err)
		}
		child.brake, child.decisions = brake, decisions
	}
	scriptRunWatch = newRunWatch()
	go child.outbox.run(func(err error) { child.calls.fail(err) })
	go child.beat()
	return child.serve()
}

func envOr(key string, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func workerConfigFromEnv() config.Config {
	result := config.Config{ProtocolSegmentName: "payload", JsTimeout: defaultJsTimeout, StateFlushInterval: defaultStateFlushInterval}
	if value, err := time.ParseDuration(os.Getenv("JS_TIMEOUT")); err == nil {
		result.JsTimeout = value
	}
	if value, err := time.ParseDuration(os.Getenv("STATE_FLUSH_INTERVAL")); err == nil {
		result.StateFlushInterval = value
	}
	if value, err := strconv.Atoi(os.Getenv("PUBLISH_WORKERS")); err == nil {
		result.PublishWorkers = value
	}
	return result
}

func (this *workerChild) serve() int {
	for {
		msg, err := this.conn.read()
		if err != nil {
			//the supervisor is gone or the stream is corrupt: there is nobody left to flush to
			util.Logger.Error("the worker lost its supervisor", attributes.ErrorKey, err)
			this.calls.fail(err)
			return 3
		}
		switch msg.Kind {
		case wireReply:
			this.calls.deliver(msg)
		case wireAcks:
			for _, ack := range msg.Acks {
				this.calls.deliver(&wireMsg{Kind: wireReply, Id: ack.Seq, Err: ack.Err})
			}
		case wireStart:
			go func() { this.reply(msg, &wireMsg{Decisions: this.decisions}, this.start(msg)) }()
		case wireProfile:
			go func() {
				result, err := this.profile(msg)
				this.reply(msg, &wireMsg{Profile: result}, err)
			}()
		case wireSetState:
			go func() { this.reply(msg, &wireMsg{}, this.setState(msg)) }()
		case wireSnapshot:
			go func() {
				blob, err := this.snapshot(msg)
				this.reply(msg, &wireMsg{Blob: blob}, err)
			}()
		case wireStop:
			go this.stopAndExit(msg)
		default:
			util.Logger.Warn("the worker got a frame it does not know", "kind", msg.Kind)
		}
	}
}

func (this *workerChild) reply(request *wireMsg, answer *wireMsg, err error) {
	answer.Kind = wireReply
	answer.Id = request.Id
	answer.Err = toWireError(err)
	this.outbox.send(answer)
}

// stopAndExit writes its reply past the outbox, so it is on the pipe before the exit.
func (this *workerChild) stopAndExit(request *wireMsg) {
	this.mux.Lock()
	rt := this.rt
	this.mux.Unlock()
	if rt != nil {
		rt.Stop()
	}
	if this.brake != nil {
		this.brake.Shutdown()
	}
	_ = this.conn.write(&wireMsg{Kind: wireReply, Id: request.Id})
	os.Exit(0)
}

func (this *workerChild) beat() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if !this.outbox.send(&wireMsg{Kind: wireBeatKind, Beat: &wireBeat{
			InRunNanos: int64(scriptRunWatch.oldest()),
			Runs:       scriptRunWatch.runs.Load(),
		}}) {
			return
		}
	}
}

func (this *workerChild) start(msg *wireMsg) error {
	var def domain.Environment
	if err := bson.Unmarshal(msg.Blob, &def); err != nil {
		return err
	}
	this.mux.Lock()
	defer this.mux.Unlock()
	if this.rt != nil {
		return errors.New("the worker already hosts an environment")
	}
	this.envs = &workerEnvironments{def: def}
	rt := newRuntime(this.config, this.envs, &workerStates{child: this}, nil, newWorkerHistoryJobs(), &workerPublisher{child: this})
	rt.brake = this.brake
	if err := rt.Start(context.Background()); err != nil {
		return err
	}
	rt.mux.RLock()
	_, running := rt.envs[def.Id]
	rt.mux.RUnlock()
	if !running {
		rt.Stop()
		return errors.New("the environment did not start, see the worker log")
	}
	this.rt = rt
	return nil
}

func (this *workerChild) runtime() (*Runtime, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if this.rt == nil {
		return nil, repo.ErrNotRunning
	}
	return this.rt, nil
}

func (this *workerChild) setState(msg *wireMsg) error {
	rt, err := this.runtime()
	if err != nil {
		return err
	}
	var change repo.StateChange
	if err := bson.Unmarshal(msg.Blob, &change); err != nil {
		return err
	}
	change.Context = plainMap(change.Context)
	for id, values := range change.Zones {
		change.Zones[id] = plainMap(values)
	}
	for id, values := range change.Assets {
		change.Assets[id] = plainMap(values)
	}
	return rt.SetState(msg.Env, change)
}

func plainMap(values map[string]interface{}) map[string]interface{} {
	for key, value := range values {
		values[key] = plainBsonValue(value)
	}
	return values
}

func (this *workerChild) snapshot(msg *wireMsg) ([]byte, error) {
	rt, err := this.runtime()
	if err != nil {
		return nil, err
	}
	snapshot, err := rt.Snapshot(msg.Env)
	if err != nil {
		return nil, err
	}
	return bson.Marshal(snapshot)
}

// profile is TestProfileTheHistoryRunOfADocument inside the worker: the same
// engine call over the same window, with the publisher on the far side of the pipe.
func (this *workerChild) profile(msg *wireMsg) (*wireProfileResult, error) {
	var def domain.Environment
	if err := bson.Unmarshal(msg.Blob, &def); err != nil {
		return nil, err
	}
	from := time.Unix(0, msg.From).UTC()
	to := time.Unix(0, msg.To).UTC()
	rt := newRuntime(this.config, &workerEnvironments{def: def}, &workerStates{child: this}, nil, newWorkerHistoryJobs(), &workerPublisher{child: this})
	gen := newGeneration(def, map[string]replaySeries{})
	env := &environment{id: def.Id, gen: gen, state: repo.RuntimeState{EnvironmentId: def.Id}}
	env.resetForHistory()
	env.seed(gen, from)

	result := &wireProfileResult{}
	var checkpoint historyCheckpointFunc
	if msg.Flags&wireProfileCheckpoints != 0 {
		checkpoint = func(progress repo.HistoryJobProgress) error {
			blob, err := bson.Marshal(progress)
			if err != nil {
				return err
			}
			result.Checkpoints++
			result.CheckpointBytes += int64(len(blob))
			_, err = this.call(&wireMsg{Kind: wireCheckpoint, Env: def.Id, Blob: blob}, storeTimeout)
			return err
		}
	}
	if this.batchInstants {
		this.outbox.hold.Store(true)
		defer func() {
			this.outbox.hold.Store(false)
			this.outbox.flushNow()
		}()
	}
	started := time.Now()
	history, err := rt.runHistory(context.Background(), env, gen, from, to, keepTheWindow, nil, nil, checkpoint)
	result.ElapsedNanos = int64(time.Since(started))
	if err != nil {
		return nil, err
	}
	result.Channels = len(history.Channels)
	for _, channel := range history.Channels {
		result.Steps += channel.Published + channel.Silent + channel.Failed
	}
	result.Published = history.Published
	result.Failed = history.Failed
	return result, nil
}

// call is a request to the supervisor; timeout bounds the wait for its reply.
func (this *workerChild) call(msg *wireMsg, timeout time.Duration) (*wireMsg, error) {
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
		return nil, errors.New("the supervisor did not answer in time")
	}
}

// publish blocks until the supervisor acknowledges the reading, so the ack
// stays per reading exactly as with the connector in process.
func (this *workerChild) publish(device string, service string, value interface{}, at time.Time, timed bool) error {
	reading := wireReading{Device: device, Service: service, Timed: timed}
	if timed {
		reading.At = at.UnixNano()
	}
	//encoded by the caller, which still holds whatever lock guards the value
	if err := encodeReadingValue(value, &reading); err != nil {
		return err
	}
	id, reply, err := this.calls.open()
	if err != nil {
		return err
	}
	reading.Seq = id
	if !this.outbox.sendReading(reading) {
		this.calls.forget(id)
		return io.ErrClosedPipe
	}
	answer := <-reply
	return fromWireError(answer.Err)
}

func budget(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		return time.Until(deadline)
	}
	return storeTimeout
}

type workerPublisher struct{ child *workerChild }

func (this *workerPublisher) PublishEvent(device string, service string, value interface{}) error {
	return this.child.publish(device, service, value, time.Time{}, false)
}

func (this *workerPublisher) PublishEventAt(device string, service string, value interface{}, at time.Time) error {
	return this.child.publish(device, service, value, at, true)
}

func (this *workerPublisher) TimeShapeOf(device string, service string) (devices.TimeShape, error) {
	answer, err := this.child.call(&wireMsg{Kind: wireTimeShape, Device: device, Service: service}, storeTimeout)
	if err != nil {
		return devices.TimeShape{}, err
	}
	if answer.Shape == nil {
		return devices.TimeShape{}, errors.New("the supervisor answered no time shape")
	}
	return *answer.Shape, nil
}

// workerStates keeps the state in the supervisor, which owns the store.
type workerStates struct{ child *workerChild }

func (this *workerStates) Load(ctx context.Context, id string) (repo.RuntimeState, error) {
	answer, err := this.child.call(&wireMsg{Kind: wireLoadState, Env: id}, budget(ctx))
	if err != nil {
		return repo.RuntimeState{}, err
	}
	state := repo.RuntimeState{}
	if len(answer.Blob) > 0 {
		if err := bson.Unmarshal(answer.Blob, &state); err != nil {
			return repo.RuntimeState{}, err
		}
	}
	state.EnvironmentId = id
	state.Context = plainMap(state.Context)
	if state.Context == nil {
		state.Context = map[string]interface{}{}
	}
	if state.Zones == nil {
		state.Zones = map[string]map[string]interface{}{}
	}
	if state.Assets == nil {
		state.Assets = map[string]map[string]interface{}{}
	}
	for zone, values := range state.Zones {
		state.Zones[zone] = plainMap(values)
	}
	for asset, values := range state.Assets {
		state.Assets[asset] = plainMap(values)
	}
	return state, nil
}

func (this *workerStates) Save(ctx context.Context, state repo.RuntimeState) error {
	blob, err := bson.Marshal(state)
	if err != nil {
		return err
	}
	_, err = this.child.call(&wireMsg{Kind: wireSaveState, Env: state.EnvironmentId, Blob: blob}, budget(ctx))
	return err
}

func (this *workerStates) Delete(ctx context.Context, id string) error {
	_, err := this.child.call(&wireMsg{Kind: wireDeleteState, Env: id}, budget(ctx))
	return err
}

// workerEnvironments holds the one definition the supervisor handed over.
type workerEnvironments struct {
	mux sync.Mutex
	def domain.Environment
}

var errNotInWorker = errors.New("not available inside an environment worker")

func (this *workerEnvironments) Put(context.Context, domain.Environment) (int64, error) {
	return 0, errNotInWorker
}

func (this *workerEnvironments) PutIfVersion(context.Context, domain.Environment, int64) (int64, error) {
	return 0, errNotInWorker
}

func (this *workerEnvironments) Get(_ context.Context, id string) (domain.Environment, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if id != this.def.Id {
		return domain.Environment{}, repo.ErrNotFound
	}
	return this.def, nil
}

func (this *workerEnvironments) ListByOwner(context.Context, string) ([]domain.Environment, error) {
	return nil, errNotInWorker
}

func (this *workerEnvironments) All(context.Context) ([]domain.Environment, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	return []domain.Environment{this.def}, nil
}

func (this *workerEnvironments) Delete(context.Context, string) error { return errNotInWorker }

// workerHistoryJobs is memory only: the spike does not carry a stored run
// across a worker restart (see the report; production would forward it).
type workerHistoryJobs struct {
	mux     sync.Mutex
	records map[string][]byte
}

func newWorkerHistoryJobs() *workerHistoryJobs {
	return &workerHistoryJobs{records: map[string][]byte{}}
}

func (this *workerHistoryJobs) Save(_ context.Context, record repo.HistoryJobRecord) error {
	blob, err := bson.Marshal(record)
	if err != nil {
		return err
	}
	this.mux.Lock()
	this.records[record.EnvironmentId] = blob
	this.mux.Unlock()
	return nil
}

func (this *workerHistoryJobs) Checkpoint(_ context.Context, id string, _ repo.HistoryJobProgress) error {
	this.mux.Lock()
	defer this.mux.Unlock()
	if _, known := this.records[id]; !known {
		return repo.ErrNotFound
	}
	return nil
}

func (this *workerHistoryJobs) Load(_ context.Context, id string) (repo.HistoryJobRecord, error) {
	this.mux.Lock()
	blob, known := this.records[id]
	this.mux.Unlock()
	if !known {
		return repo.HistoryJobRecord{}, repo.ErrNotFound
	}
	var record repo.HistoryJobRecord
	err := bson.Unmarshal(blob, &record)
	return record, err
}

func (this *workerHistoryJobs) Running(context.Context) ([]repo.HistoryJobRecord, error) {
	return nil, nil
}

func (this *workerHistoryJobs) Delete(_ context.Context, id string) error {
	this.mux.Lock()
	delete(this.records, id)
	this.mux.Unlock()
	return nil
}
