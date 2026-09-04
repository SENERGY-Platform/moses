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
	"errors"
	"fmt"
	"hash/fnv"
	"reflect"
	"sync"
	"time"

	"github.com/SENERGY-Platform/platform-connector-lib/model"
)

// The publish pool lets a history run or a backfill have several readings in
// flight while every one of them is still sent through the same synchronous
// call, with its own ack and its own error. A channel is pinned to one worker,
// so the readings of one channel keep the order they were computed in.
//
// Submit never waits, because it is called with the environment mutex held; the
// backpressure is Throttle, which the loop calls between two instants with no
// mutex held. docs/history-run.md says what the throughput is bound by.

// defaultPublishWorkers is the pool size for a configuration that leaves the
// count at zero, maxPublishWorkers the ceiling: every worker holds its share of
// the outstanding readings, each carrying a full channel binding, so a count far
// above the channels of an environment only costs memory.
const (
	defaultPublishWorkers = 16
	maxPublishWorkers     = 256
)

// publishQueuePerWorker is what one worker may have staged, and its share of the
// mark over the whole pool. Throttle holds both, because a run over a single
// channel uses one worker: without the per-worker bound it would stage the mark
// of the whole pool there, and an abort would drop all of it. A larger share
// cannot make one worker publish faster.
const publishQueuePerWorker = 32

// maxPublishCopyDepth bounds the copy below. A reading nested deeper than this
// is not something anyone models, and the bound is what keeps a cyclic value a
// script built from taking the stack down.
const maxPublishCopyDepth = 32

// publishWarmMux and publishWarm are process wide rather than per pool: the map
// the connector library writes on a first produce is one for the process, and
// two runs of two environments have two pools.
var (
	publishWarmMux sync.RWMutex
	publishWarm    = map[string]bool{}
)

// ErrPublishAborted is what a reading is failed with that was accepted but never
// sent, because the run was cancelled or the service is shutting down. It is
// booked as silent, not as failed: nothing was refused.
var ErrPublishAborted = errors.New("the run was aborted before this reading was published")

// publishJob is one reading on its way to the platform. channelId pins it to a
// worker; done is called exactly once, on the worker that sends, and must not
// take the environment mutex - the caller may hold it while it submits.
type publishJob struct {
	channelId string
	binding   channelBinding
	value     interface{}
	at        time.Time
	done      func(sent bool, err error)
}

// publishShard is the staging list of one worker. It is unbounded on purpose:
// the submitter may not wait, so what limits it is Throttle.
type publishShard struct {
	mux     sync.Mutex
	arrived *sync.Cond
	pending []publishJob
	closed  bool
}

// publishPool is a fixed set of workers, each with its own shard.
//
// Submit, Drain, Throttle and Close belong to the one goroutine that computes: a
// Submit after Close would stage a reading nobody sends, and Drain means
// "everything submitted so far".
type publishPool struct {
	ctx     context.Context
	cancel  context.CancelFunc
	publish func(job publishJob) (bool, error)
	shards  []*publishShard
	stopped chan struct{}

	// open counts the readings that were submitted and not yet acked, and
	// openCond wakes Drain and Throttle. mark is what Throttle holds open under,
	// recent the shard of the last submit, which it holds under the per-worker
	// share. Only the submitting goroutine touches recent.
	//
	// The one nesting of locks in this file is openMux before a shard mutex, in
	// Throttle; nothing takes them the other way round.
	openMux  sync.Mutex
	openCond *sync.Cond
	open     int
	mark     int
	recent   *publishShard

	workers sync.WaitGroup

	// panicked holds the first panic a worker saw, to be raised again on the
	// goroutine that owns the run - swallowed it would leave the job reporting
	// itself done, left on the worker it would take the service down.
	panicMux sync.Mutex
	panicked any

	closeOnce sync.Once
}

// newPublishPool starts the workers. publish is the send itself, so the pool
// stays free of what a reading is and where it goes.
func newPublishPool(ctx context.Context, workers int, publish func(job publishJob) (bool, error)) *publishPool {
	if workers < 1 {
		workers = 1
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	pool := &publishPool{
		ctx:     ctx,
		cancel:  cancel,
		publish: publish,
		shards:  make([]*publishShard, workers),
		stopped: make(chan struct{}),
		mark:    publishQueuePerWorker * workers,
	}
	pool.openCond = sync.NewCond(&pool.openMux)
	for i := range pool.shards {
		shard := &publishShard{}
		shard.arrived = sync.NewCond(&shard.mux)
		pool.shards[i] = shard
		pool.workers.Add(1)
		go pool.work(shard)
	}
	//an abort has to wake a Throttle that is waiting for capacity, or the loop
	//would keep waiting for workers that no longer send
	pool.workers.Add(1)
	go func() {
		defer pool.workers.Done()
		select {
		case <-ctx.Done():
		case <-pool.stopped:
		}
		pool.openMux.Lock()
		pool.openCond.Broadcast()
		pool.openMux.Unlock()
	}()
	return pool
}

// shardOf is the worker one channel belongs to. A hash rather than a counter, so
// the assignment follows from the channel alone and cannot change mid-run.
func (this *publishPool) shardOf(channelId string) int {
	hash := fnv.New32a()
	//Write on hash.Hash32 never returns an error
	_, _ = hash.Write([]byte(channelId))
	return int(hash.Sum32() % uint32(len(this.shards)))
}

// Submit stages one reading for the worker of its channel and returns at once.
//
// It must never wait: it is called from inside a dispatch, with the environment
// mutex held, and the flusher takes that mutex for every environment in turn -
// so a submit that waited for the platform would stop the state of every other
// environment from being written.
func (this *publishPool) Submit(job publishJob) {
	this.raise()
	//a script that read its value out of the state sent the very Go map that is
	//in the state, and the next step writes into it while the worker marshals it
	job.value = copyPublished(job.value)

	this.openMux.Lock()
	this.open++
	this.openMux.Unlock()

	shard := this.shards[this.shardOf(job.channelId)]
	shard.mux.Lock()
	if shard.closed {
		//a submit after Close is a bug in the caller; failing the reading keeps
		//the counters adding up instead of leaving a later drain waiting for a
		//worker that has already gone
		shard.mux.Unlock()
		//answered before it is counted down, the way handle does it: a drain must
		//never see nothing outstanding while a callback is still to come
		job.done(false, ErrPublishAborted)
		this.finished()
		return
	}
	shard.pending = append(shard.pending, job)
	shard.arrived.Signal()
	shard.mux.Unlock()
	this.recent = shard
}

// Throttle is the backpressure. The loop calls it between two instants with no
// mutex held, so what a run holds in memory is one instant plus the mark rather
// than the whole window. It gives up when the run is over, or an abort would
// leave it waiting for workers that no longer send.
func (this *publishPool) Throttle() {
	this.openMux.Lock()
	defer this.openMux.Unlock()
	for this.ctx.Err() == nil && (this.open >= this.mark || this.stagedRecently() >= publishQueuePerWorker) {
		this.openCond.Wait()
	}
}

// stagedRecently is how many readings wait for the worker of the channel that
// was submitted last.
func (this *publishPool) stagedRecently() int {
	if this.recent == nil {
		return 0
	}
	this.recent.mux.Lock()
	defer this.recent.mux.Unlock()
	return len(this.recent.pending)
}

// Drain returns once every reading submitted so far has been acked. It is what
// has to happen before a run reads its counters, measures how far it lags the
// clock, or hands the environment back to the live simulation.
func (this *publishPool) Drain() {
	this.openMux.Lock()
	for this.open > 0 {
		this.openCond.Wait()
	}
	this.openMux.Unlock()
	this.raise()
}

// raise re-raises a panic a worker saw, on the caller's goroutine. Called at a
// submit and at a drain, and deliberately not in Close, which runs as a defer
// while another panic may already be unwinding.
func (this *publishPool) raise() {
	this.panicMux.Lock()
	problem := this.panicked
	this.panicked = nil
	this.panicMux.Unlock()
	if problem != nil {
		panic(fmt.Errorf("a publish of this run panicked: %v", problem))
	}
}

// Abort stops the sending: everything still staged is failed rather than
// published. A run that broke calls it before Close, because the window it
// computed is half done and the comparison base of those readings is never
// booked.
func (this *publishPool) Abort() {
	this.cancel()
}

// Close ends the pool and returns once every accepted reading has been acked. It
// waits, it does not discard: a caller that wants the staged readings dropped
// aborts first. Idempotent, and safe as a defer next to a panic.
func (this *publishPool) Close() {
	this.closeOnce.Do(func() {
		close(this.stopped)
		for _, shard := range this.shards {
			shard.mux.Lock()
			shard.closed = true
			shard.arrived.Broadcast()
			shard.mux.Unlock()
		}
	})
	this.workers.Wait()
	//after the workers, so it cannot turn Close into an abort; it releases the
	//context this pool derived
	this.cancel()
}

// work is one worker. It keeps taking readings until its shard is closed and
// empty, an aborted run included: everything that was staged is answered.
func (this *publishPool) work(shard *publishShard) {
	defer this.workers.Done()
	for {
		job, ok := shard.take()
		if !ok {
			return
		}
		this.handle(job)
	}
}

// take waits for the next reading of this shard and reports false once the shard
// is closed and empty.
func (this *publishShard) take() (publishJob, bool) {
	this.mux.Lock()
	defer this.mux.Unlock()
	for len(this.pending) == 0 && !this.closed {
		this.arrived.Wait()
	}
	if len(this.pending) == 0 {
		return publishJob{}, false
	}
	job := this.pending[0]
	if len(this.pending) == 1 {
		//released rather than resliced, so a run does not keep the array of its
		//longest burst for the rest of the window
		this.pending = nil
	} else {
		this.pending = this.pending[1:]
	}
	return job, true
}

// handle publishes one reading and reports what became of it. The count is given
// back through a defer, so a bug in the bookkeeping cannot leave a Drain waiting
// for a reading that will never be acked.
func (this *publishPool) handle(job publishJob) {
	defer this.finished()
	if this.ctx.Err() != nil {
		//nothing new is sent after an abort, but every accepted reading is still
		//accounted for
		job.done(false, ErrPublishAborted)
		return
	}
	sent, err := this.sendWarming(job)
	job.done(sent, err)
}

func (this *publishPool) finished() {
	this.openMux.Lock()
	this.open--
	this.openCond.Broadcast()
	this.openMux.Unlock()
}

// sendWarming publishes, and the first reading of a service topic publishes
// alone: the connector library keeps the topics it created in a map it neither
// locks for the write nor for the read every later publish makes, so a first
// produce next to any other produce is a fatal map access (SNRGY-4664). The
// lock is process wide, so two runs cannot produce a first reading at once
// either.
//
// Marked warm only for a reading that really went out: a publish that failed
// before the produce, for want of a token, never created the topic.
func (this *publishPool) sendWarming(job publishJob) (bool, error) {
	topic := model.ServiceIdToTopic(job.binding.channel.ExternalRef)

	publishWarmMux.RLock()
	if publishWarm[topic] {
		defer publishWarmMux.RUnlock()
		return this.send(job)
	}
	publishWarmMux.RUnlock()

	publishWarmMux.Lock()
	defer publishWarmMux.Unlock()
	if publishWarm[topic] {
		//another worker warmed the topic while this one waited
		return this.send(job)
	}
	sent, err := this.send(job)
	if sent {
		publishWarm[topic] = true
	}
	return sent, err
}

// send is the publish with a recover around it: the reading is failed so the
// counters of its step add up, and the panic is kept for raise to hand to the
// run.
func (this *publishPool) send(job publishJob) (sent bool, err error) {
	defer func() {
		problem := recover()
		if problem == nil {
			return
		}
		this.panicMux.Lock()
		if this.panicked == nil {
			this.panicked = problem
		}
		this.panicMux.Unlock()
		sent, err = false, fmt.Errorf("the publish panicked: %v", problem)
	}()
	return this.publish(job)
}

// copyPublished is the reading as the pool may keep it: maps and slices are
// copied recursively, everything else is taken as it is. A script's value is
// often the very map that sits in the environment state, and a worker marshals
// it outside the mutex that state is written under.
func copyPublished(value interface{}) interface{} {
	if value == nil {
		return nil
	}
	source := reflect.ValueOf(value)
	switch source.Kind() {
	case reflect.Map, reflect.Slice, reflect.Array:
		return copyPublishedValue(source, 0).Interface()
	default:
		return value
	}
}

func copyPublishedValue(source reflect.Value, depth int) reflect.Value {
	if depth >= maxPublishCopyDepth {
		return source
	}
	switch source.Kind() {
	case reflect.Interface:
		if source.IsNil() {
			return source
		}
		copied := reflect.New(source.Type()).Elem()
		copied.Set(copyPublishedValue(source.Elem(), depth+1))
		return copied
	case reflect.Map:
		if source.IsNil() {
			return source
		}
		copied := reflect.MakeMapWithSize(source.Type(), source.Len())
		iterator := source.MapRange()
		for iterator.Next() {
			copied.SetMapIndex(iterator.Key(), copyPublishedValue(iterator.Value(), depth+1))
		}
		return copied
	case reflect.Slice:
		if source.IsNil() {
			return source
		}
		copied := reflect.MakeSlice(source.Type(), source.Len(), source.Len())
		for i := 0; i < source.Len(); i++ {
			copied.Index(i).Set(copyPublishedValue(source.Index(i), depth+1))
		}
		return copied
	case reflect.Array:
		copied := reflect.New(source.Type()).Elem()
		for i := 0; i < source.Len(); i++ {
			copied.Index(i).Set(copyPublishedValue(source.Index(i), depth+1))
		}
		return copied
	default:
		return source
	}
}
