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
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/util"
)

// A history run simulates an environment from a past instant up to the present
// on a virtual clock, publishes every reading under the instant it was computed
// for, and leaves the state it arrives at as the live state. Unlike a backfill
// it carries state - counters, schedule anchors, script state, the change
// trigger's comparison base - which is what makes the transition a ramp instead
// of a second one starting next to the first. See docs/history-run.md.

// historyProgressEvery is how many due events pass between two progress reports.
const historyProgressEvery = 1000

// historyChunkSeconds is the length of one chunk of virtual time. The run drains
// its pool at every multiple of it on the unix clock and writes a checkpoint
// there, so a crash inside a chunk republishes at most this much of the window
// when the run is resumed. It stays a constant until the profile test says
// otherwise; see docs/history-run.md.
const historyChunkSeconds = 3600

// chaseTheClock and keepTheWindow are what the chase parameter of the engine
// means at a call site: a run whose end was the present when it was asked for
// keeps going until it has caught up, a run over a window that had already
// passed simulates exactly that window.
const (
	chaseTheClock = true
	keepTheWindow = false
)

// historyCatchUpSettled is the gap below which chasing stops: the handover costs
// more than this, so closing it further would be chasing the clock forever.
const historyCatchUpSettled = time.Second

// historyCatchUpRounds is a backstop only. The real bound is the halving in
// historyChasesOn, which keeps the whole chase to about twice the pass that
// preceded it; this catches a clock that jumps.
const historyCatchUpRounds = 32

// historyChasesOn says whether another catch-up round is worth running. Each
// round has to close at least half of what was left, so that the rounds sum to
// about twice the first one instead of running until the cap: a chase that only
// shrinks the gap a little would multiply the work the volume check bounded by
// the number of rounds allowed.
//
// A run that publishes slower than half of real time therefore stops with a gap
// still open. That is the honest outcome - it will never close it - and the
// position in the status says how much is missing.
func historyChasesOn(gap time.Duration, lastGap time.Duration) bool {
	if gap <= historyCatchUpSettled {
		return false
	}
	//halved rather than doubled, so a first gap near the largest duration cannot
	//overflow the comparison
	return gap <= lastGap/2
}

// historyProgress is how the engine reports where it stands. published and
// failed are the running totals over every channel.
type historyProgress func(at time.Time, published int64, failed int64, lastError string)

// historyCheckpointFunc stores where a run stands at a chunk boundary, complete
// enough to continue from. A nil function means the run is not remembered, which
// is what the engine's own tests pass.
type historyCheckpointFunc func(progress repo.HistoryJobProgress) error

// historyEngineFunc is the seam between the run and the lifecycle around it.
// resume is nil for a fresh run and otherwise the checkpoint the run continues
// from, whose state and value cache the caller has already put into the
// environment.
type historyEngineFunc func(ctx context.Context, env *environment, gen *generation, from time.Time, to time.Time, chase bool, progress historyProgress, resume *repo.HistoryCheckpoint, checkpoint historyCheckpointFunc) (HistoryResult, error)

// HistoryChannelStatus is what became of one channel of the run.
//
// Published, Silent and Failed count the steps of the channel's publish grid,
// exactly one of them per step: published when at least one reading of that step
// went out, failed when one was attempted and none did, silent when the step
// sent nothing at all. A channel that cannot publish is silent throughout and
// says why in Reason - it still computes, because everything else in the
// environment reads what it produces.
type HistoryChannelStatus struct {
	ChannelId string `json:"channel_id"`
	AssetId   string `json:"asset_id"`
	Name      string `json:"name"`

	Publishable bool   `json:"publishable"`
	Reason      string `json:"reason,omitempty"`

	Published int64  `json:"published"`
	Silent    int64  `json:"silent,omitempty"`
	Failed    int64  `json:"failed,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// HistoryResult is what one run produced.
type HistoryResult struct {
	Channels []HistoryChannelStatus

	Published int64
	Failed    int64
	LastError string

	// Position is the last virtual instant the run reached, End the instant it
	// ran up to. End moves past the requested end while the run chases the clock
	// it lost while it was simulating.
	Position time.Time
	End      time.Time
}

// historyShared is what the whole run keeps rather than one channel: the pool
// and the message of the most recent publish failure.
//
// mux guards lastError and the counters of every channel, which the pool's
// workers book while the loop computes - so a polled status may lag by the acks
// in flight, while the totals of a drained run are exact. It is a leaf lock,
// never held while a reading is submitted.
type historyShared struct {
	pool *publishPool

	mux       sync.Mutex
	lastError string

	// dropped counts the readings the pool accepted and never sent, which an
	// abort or a shutdown leaves behind. Their steps are booked as silent and
	// their grids have moved on, so a checkpoint at the abort position would
	// claim they were covered - see the abort in runHistory.
	dropped int64
}

// droppedCount is how many readings the pool has failed without sending. A
// caller that has drained and settled reads a number that cannot move again
// until the next submit.
func (this *historyShared) droppedCount() int64 {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.dropped
}

// historyClass orders the events that fall on the same instant. The live
// runtime picks one of these orders at random, per select; the run fixes one, so
// that the same document and window produce the same series twice.
//
// Everything a channel reads moves before the channel: a context source before
// the channels that read it, a producing channel before the formulas and
// aggregates that are derived from what it just produced, and the source half of
// a split channel before its publish half. Only a derivation over a derivation
// still reads the previous instant, since both sit in the same class.
const (
	historyClassContext = iota
	historyClassProducer
	historyClassDerived
	historyClassPublish
)

// historyClassOf says whether a channel produces a value of its own or is
// derived from what the others produced in this instant.
func historyClassOf(channel domain.Channel) int {
	switch channel.Source.Kind {
	case domain.SourceFormula, domain.SourceAggregate:
		return historyClassDerived
	}
	return historyClassProducer
}

// historyGrid is one cadence of the run - a context source, a channel, or the
// publish half of a split channel. tick is the next step that has not run yet,
// which is what lets a grid be picked up again when the run chases the clock.
//
// id is the identity a checkpoint keys the tick by - "context:<key>",
// "<channel id>" or "<channel id>:publish" - rather than the position in the
// slice, which an edit to the document would shift.
type historyGrid struct {
	id    string
	class int
	order int
	step  int64
	tick  int64
}

// historyEvent is one due date on the virtual clock. tick counts whole steps
// from the window start, so at is built by multiplication rather than by
// repeated addition and cannot drift over a year long window.
type historyEvent struct {
	grid    int
	class   int
	order   int
	dueUnix int64
	at      time.Time
}

// historyQueue is the min-heap over (dueUnix, class, order). The tie-break is
// total: two events never compare equal, since class and order together
// identify one source.
type historyQueue []historyEvent

func (this historyQueue) Len() int { return len(this) }

func (this historyQueue) Less(i int, j int) bool {
	if this[i].dueUnix != this[j].dueUnix {
		return this[i].dueUnix < this[j].dueUnix
	}
	if this[i].class != this[j].class {
		return this[i].class < this[j].class
	}
	return this[i].order < this[j].order
}

func (this historyQueue) Swap(i int, j int) { this[i], this[j] = this[j], this[i] }

func (this *historyQueue) Push(x any) { *this = append(*this, x.(historyEvent)) }

func (this *historyQueue) Pop() any {
	old := *this
	last := len(old) - 1
	item := old[last]
	*this = old[:last]
	return item
}

// historyChannel is one channel as the run drives it.
type historyChannel struct {
	binding channelBinding

	// pending is the value the last computation produced, kept rather than
	// consumed, exactly as the live split and change-trigger runners keep it.
	pending latest

	// heartbeatSeconds and lastAttemptUnix reproduce the heartbeat timer of a
	// channel publishing on change as a condition on the grid. lastAttemptUnix
	// starts at the window start, because live the timer starts with the channel.
	// The comparison base is not tracked here: it is the persisted one, which is
	// what makes the run and the live channel that follows it agree.
	heartbeatSeconds int64
	lastAttemptUnix  int64

	// faultMemory is what the channel's injected faults remember for the length
	// of the run, exactly as a live runner keeps its own. The captured meter
	// offsets are not here: those live in the environment state the run hands
	// over, so the live channel continues from the register the run left.
	faultMemory *faultRun

	// outstanding is the one publish of this channel whose answer has not been
	// collected yet, acks where the worker leaves it. A change trigger decides
	// against the base and the gap the previous publish left, so every decision
	// collects the answer first - which is what keeps at most one open.
	outstanding *covOutstanding
	acks        chan bool

	// reported keeps the log to one line per broken channel.
	reported bool

	// shared carries what belongs to the run rather than to this channel.
	shared *historyShared

	result HistoryChannelStatus
}

// covOutstanding is a publish of a channel with a change trigger that has not
// been acked yet: what the gate applies once the answer is there.
type covOutstanding struct {
	at      time.Time
	number  float64
	numeric bool
	// forced tells a heartbeat publish from a change publish: a heartbeat
	// restarts the gap on the attempt, a change publish only when it went out.
	forced bool
}

// historyOutcome is what one step of one channel did, before it is folded into
// the three counters.
type historyOutcome struct {
	attempted int
	published int
}

func (this *historyChannel) record(outcome historyOutcome) {
	this.shared.mux.Lock()
	defer this.shared.mux.Unlock()
	switch {
	case outcome.attempted == 0:
		this.result.Silent++
	case outcome.published > 0:
		this.result.Published++
	default:
		this.result.Failed++
	}
}

// historyStep collects the attempts of one step of one channel and books it
// exactly once, when the last of them has been acked: one step can carry several
// attempts, and published + silent + failed has to stay the number of steps. The
// loop has moved on by then, which is why the step is passed down rather than an
// outcome the caller reads afterwards.
type historyStep struct {
	channel *historyChannel

	mux       sync.Mutex
	attempted int
	acked     int
	published int
	// aborted counts the attempts the pool dropped without sending them, which
	// count as never attempted: silent means nothing was tried, failed means the
	// platform refused.
	aborted int
	// sealed says the loop is done submitting attempts for this step. Whichever
	// of sealing and the last ack happens second books the step.
	sealed bool
	booked bool
}

// attempt registers one publish, before the reading is submitted: the counter
// must never be reached by an ack it does not know about yet.
func (this *historyStep) attempt() {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.attempted++
}

// ack takes the answer of one attempt.
func (this *historyStep) ack(sent bool, aborted bool) {
	this.mux.Lock()
	this.acked++
	if sent {
		this.published++
	}
	if aborted {
		this.aborted++
	}
	this.mux.Unlock()
	this.book()
}

// seal ends the step on the loop's side.
func (this *historyStep) seal() {
	this.mux.Lock()
	this.sealed = true
	this.mux.Unlock()
	this.book()
}

func (this *historyStep) book() {
	this.mux.Lock()
	if this.booked || !this.sealed || this.acked < this.attempted {
		this.mux.Unlock()
		return
	}
	this.booked = true
	outcome := historyOutcome{attempted: this.attempted - this.aborted, published: this.published}
	this.mux.Unlock()
	//outside this mutex: record takes the run's own, and holding two where one
	//would do is how a lock order gets invented
	this.channel.record(outcome)
}

// runHistory simulates the environment from from to to on a virtual clock.
//
// chase says whether the run keeps going past to once it has drained that
// window, which the caller knows and the engine cannot guess: to was the present
// when the run was asked for, and a run of any length has moved on from it by
// the time it gets there.
//
// It must be called with the environment reset and seeded and with its live
// runners stopped: it holds no lock of its own, and the executors it drives take
// the environment mutex themselves.
//
// resume continues an interrupted run: the caller has put its state and value
// cache into the environment, the engine takes ticks and channel memory from it,
// and from and to have to be the original window since a tick counts from from.
// checkpoint is called at every chunk boundary and at an abort with everything
// quiescent; a failure is logged once and the run goes on unremembered.
func (this *Runtime) runHistory(ctx context.Context, env *environment, gen *generation, from time.Time, to time.Time, chase bool, progress historyProgress, resume *repo.HistoryCheckpoint, checkpoint historyCheckpointFunc) (HistoryResult, error) {
	result := HistoryResult{Channels: []HistoryChannelStatus{}}

	//the end is fixed here rather than read per event: inside one pass the
	//virtual clock must never overshoot the present, where scheduleAt clamps and
	//a replay goes silent. It is raised again only between passes, below.
	end := to
	if now := time.Now(); end.After(now) {
		end = now
	}
	baseUnix := from.Unix()
	endUnix := end.Unix()
	result.End = end
	if endUnix < baseUnix {
		return result, nil
	}

	keys := historyContextKeys(gen)

	// due is the k-th instant of one grid, and whether it still lies inside the
	// window. The whole seconds are compared first, and not only because they are
	// the heap's order: they also bound tick*step to the span, without which the
	// multiplication into a Duration could overflow for a grid coarser than the
	// window. The instant is then compared against the end itself, since a start
	// carrying a fraction of a second would otherwise let the last step of a grid
	// land up to a second past it - in the future, for a window that ends now.
	//
	// Built in the local zone: a profile reads the hour and the weekday off the
	// instant and the live path hands it a local clock, so a window given in UTC
	// would otherwise shift every day profile by this server's zone offset.
	due := func(step int64, tick int64) (time.Time, bool) {
		if step <= 0 || step > maxIntervalSeconds {
			return time.Time{}, false
		}
		if baseUnix+tick*step > endUnix {
			return time.Time{}, false
		}
		at := from.Add(time.Duration(tick*step) * time.Second).In(time.Local)
		if at.After(end) {
			return time.Time{}, false
		}
		return at, true
	}

	//the workers send through the same synchronous path the loop used before the
	//pool: one reading, one ack, one error. Close covers a panic as well as a
	//return, so a broken run leaves nothing in flight.
	pool := newPublishPool(ctx, this.publishWorkers, func(job publishJob) (bool, error) {
		return this.publishAt(env, job.binding, job.value, false, job.at)
	})
	defer pool.Close()
	//registered after Close, so it runs before it: a run that broke must not let
	//Close send the rest of its staged readings, whose comparison base nothing
	//will book
	defer func() {
		if problem := recover(); problem != nil {
			pool.Abort()
			panic(problem)
		}
	}()

	channels := make([]*historyChannel, len(gen.sensors))
	shared := &historyShared{pool: pool}
	//from the same function the refusal in StartHistory checks: two lists of
	//grids could drift, and a run would then stand on one nothing looked at
	grids := historyGridsOf(gen, keys)

	for i, binding := range gen.sensors {
		publishable, reason := this.historyPublishable(binding)
		if !publishable {
			//loud on purpose: a device repository that was briefly unreachable
			//makes every channel of the environment look like this, and a run that
			//then reports "done, nothing published" without a word is the failure
			//that takes longest to understand
			util.Logger.Warn("this channel of the history run computes but publishes nothing",
				"environment", gen.def.Id, "channel", binding.channel.Id, "reason", reason)
		}
		channels[i] = &historyChannel{
			binding:          binding,
			heartbeatSeconds: binding.channel.IntervalSeconds,
			lastAttemptUnix:  baseUnix,
			faultMemory:      &faultRun{},
			shared:           shared,
			//one slot, because a channel never has more than one publish open
			acks: make(chan bool, 1),
			result: HistoryChannelStatus{
				ChannelId:   binding.channel.Id,
				AssetId:     binding.asset.id,
				Name:        binding.channel.Name,
				Publishable: publishable,
				Reason:      reason,
			},
		}
	}

	position := from
	if resume != nil || checkpoint != nil {
		//the lifecycle refuses such a document before it stops anything, so this
		//is the belt of that brace: only a run that is remembered depends on the
		//identities being unique, and the check is one pass over the grids.
		if err := historyDistinctGrids(grids); err != nil {
			return result, err
		}
	}
	if resume != nil {
		if err := historyResume(grids, channels, shared, baseUnix, resume); err != nil {
			return result, err
		}
		position = resume.Position
	}

	queue := &historyQueue{}
	// admit puts the next step of every grid that is still inside the window into
	// the heap. It runs once per pass, which is what lets a grid be picked up
	// again when the end is raised.
	admit := func() {
		for gi := range grids {
			at, inside := due(grids[gi].step, grids[gi].tick)
			if !inside {
				continue
			}
			heap.Push(queue, historyEvent{
				grid: gi, class: grids[gi].class, order: grids[gi].order,
				dueUnix: baseUnix + grids[gi].tick*grids[gi].step, at: at,
			})
		}
	}
	admit()

	processed := int64(0)
	//lastGap is what the previous pass had left to close; see historyChasesOn
	lastGap := time.Duration(math.MaxInt64)

	//the first chunk boundary that has not been written yet. A resumed run
	//continues after the boundary it stood at rather than rewriting the ones
	//before it.
	nextBoundary := historyChunkAfter(baseUnix)
	if resume != nil {
		nextBoundary = historyChunkAfter(resume.Position.Unix())
	}
	//once per run: a store that is unreachable would otherwise write a line per
	//virtual hour
	checkpointReported := false
	remember := func(progress repo.HistoryJobProgress) {
		if checkpoint == nil {
			return
		}
		if err := checkpoint(progress); err != nil && !checkpointReported {
			checkpointReported = true
			util.Logger.Error("unable to remember the history run, it goes on unremembered",
				attributes.ErrorKey, err, "environment", gen.def.Id, "at", progress.Position)
		}
	}

	//abort ends the run where it stands. Everything is booked before the counters
	//are read, or the three of them would not add up to the steps the run took.
	abort := func(err error) (HistoryResult, error) {
		pool.Drain()
		this.historySettleAll(env, channels)
		result.Channels = historyResults(channels, shared)
		result.Published, result.Failed, result.LastError = historyTotals(channels, shared)
		//the end of an aborted run is the last instant it actually simulated: a
		//chase round that was cut short had already raised end, and reporting
		//that would claim a span the run never covered
		result.Position, result.End = position, position
		if checkpoint != nil {
			//asked for only where it is stored: building it copies the whole
			//state, which a run nobody remembers has no use for
			remember(this.historyAbortedAt(env, grids, channels, shared, position))
		}
		return result, err
	}

	for round := 0; ; round++ {
		for queue.Len() > 0 {
			if err := ctx.Err(); err != nil {
				return abort(err)
			}
			//the whole seconds of the next event decide, the same clock the heap is
			//ordered on: an event due exactly at a boundary belongs to the chunk
			//that starts there, so everything before the boundary is computed and
			//acked when the checkpoint is written.
			if checkpoint != nil {
				if reached := historyChunkOf((*queue)[0].dueUnix); reached >= nextBoundary {
					//an abort inside this drain drops the staged readings while their
					//grids have moved on, so a boundary written over that would lose
					//them for good: it is written only for a drain that lost nothing.
					//The dropped count is asked as well as the cancel because the panic
					//path stops the pool without one.
					droppedBefore := shared.droppedCount()
					pool.Drain()
					this.historySettleAll(env, channels)
					if ctx.Err() == nil && shared.droppedCount() == droppedBefore {
						progress := this.historyCheckpointAt(env, grids, channels, shared, time.Unix(reached, 0).In(time.Local))
						//a boundary, so the store starts the resume count over: this
						//run has got going since it was picked up
						progress.AtBoundary = true
						remember(progress)
					}
					//several boundaries at once when no due event fell between them:
					//the run stands at the same instant at each of them, so one write
					//says it
					nextBoundary = reached + historyChunkSeconds
					//back to the top rather than on to the event: an abort that
					//arrived while the boundary was being written ends the run here,
					//with nothing staged
					continue
				}
			}
			event := heap.Pop(queue).(historyEvent)
			at := event.at
			position = at

			switch event.class {
			case historyClassContext:
				key := keys[event.order]
				this.tickContextSource(env, gen, key, gen.def.ContextSources[key], at)
			case historyClassPublish:
				this.historyPublishDue(env, channels[event.order], at)
			default:
				this.historyChannelDue(env, gen, channels[event.order], at)
			}
			//the backpressure, and here rather than at the submit: the executors
			//above have released the environment mutex, so a wait for the platform
			//no longer blocks the flusher of every environment
			pool.Throttle()

			processed++
			if processed%historyProgressEvery == 0 && progress != nil {
				published, failed, lastError := historyTotals(channels, shared)
				progress(at, published, failed, lastError)
			}

			grids[event.grid].tick++
			if next, inside := due(grids[event.grid].step, grids[event.grid].tick); inside {
				event.dueUnix = baseUnix + grids[event.grid].tick*grids[event.grid].step
				event.at = next
				heap.Push(queue, event)
			}
		}

		//the backlog is part of the time the run has lost: a gap taken with
		//readings in flight would end the chase against a clock the run has not
		//caught up with
		droppedInDrain := shared.droppedCount()
		pool.Drain()
		this.historySettleAll(env, channels)
		//what makes this pass incomplete is that the drain lost readings, not that
		//the context is done: a cancel arriving after the last ack leaves a whole
		//window, and reporting that as cancelled would have a shutdown resume a run
		//that is over and simulate its last chunk again.
		if shared.droppedCount() != droppedInDrain {
			//the pool derives its context from this one, so only an end of this run
			//drops a reading; the fallback is for the pool aborting itself
			err := ctx.Err()
			if err == nil {
				err = context.Canceled
			}
			return abort(err)
		}

		//the window is drained; a long run has meanwhile lost the time it spent
		//simulating, and handing the environment over across that hole would put
		//the step back that the mode exists to avoid
		if !chase || round >= historyCatchUpRounds {
			break
		}
		//the window is complete and the chase is what closes the seam of a run
		//that is going on: a cancelled run stops here rather than turning a whole
		//window into a cancelled one over the seam
		if ctx.Err() != nil {
			break
		}
		gap := time.Since(end)
		if !historyChasesOn(gap, lastGap) {
			break
		}
		lastGap = gap
		end = time.Now()
		endUnix = end.Unix()
		admit()
	}

	//every pass above ends drained; said again where the counters are read, so a
	//further way out of the loop cannot report totals that are still moving
	pool.Drain()
	this.historySettleAll(env, channels)
	result.Channels = historyResults(channels, shared)
	result.Published, result.Failed, result.LastError = historyTotals(channels, shared)
	result.Position, result.End = position, end
	if progress != nil {
		progress(position, result.Published, result.Failed, result.LastError)
	}
	return result, nil
}

// historyResults and historyTotals read the counters the pool's workers write,
// so both take the run's mutex. A caller that wants final numbers drains first.
func historyResults(channels []*historyChannel, shared *historyShared) []HistoryChannelStatus {
	shared.mux.Lock()
	defer shared.mux.Unlock()
	result := make([]HistoryChannelStatus, 0, len(channels))
	for _, channel := range channels {
		result = append(result, channel.result)
	}
	return result
}

func historyTotals(channels []*historyChannel, shared *historyShared) (published int64, failed int64, lastError string) {
	shared.mux.Lock()
	defer shared.mux.Unlock()
	for _, channel := range channels {
		published += channel.result.Published
		failed += channel.result.Failed
	}
	return published, failed, shared.lastError
}

// historyChunkOf is the boundary at or below one instant, and historyChunkAfter
// the first one strictly above it. Both floor towards the epoch rather than
// towards zero, so a window before 1970 does not skip the boundaries between it
// and the epoch.
func historyChunkOf(unix int64) int64 {
	chunks := unix / historyChunkSeconds
	if unix%historyChunkSeconds != 0 && unix < 0 {
		chunks--
	}
	return chunks * historyChunkSeconds
}

func historyChunkAfter(unix int64) int64 {
	return historyChunkOf(unix) + historyChunkSeconds
}

// historyContextKeys is the context sources of one generation, sorted: two
// sources due at the same instant have to move in the same order on every run.
func historyContextKeys(gen *generation) []string {
	keys := make([]string, 0, len(gen.def.ContextSources))
	for key := range gen.def.ContextSources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// historyGridsOf is every cadence a run of this generation drives, with the
// identity a checkpoint keys its tick by; one function for the engine and the
// refusal in StartHistory, so the two lists cannot drift. order is the position
// in keys or in gen.sensors, the heap's tie-break.
func historyGridsOf(gen *generation, keys []string) []historyGrid {
	grids := make([]historyGrid, 0, len(keys)+2*len(gen.sensors))
	add := func(id string, class int, order int, step int64) {
		grids = append(grids, historyGrid{id: id, class: class, order: order, step: step})
	}
	for i, key := range keys {
		add("context:"+key, historyClassContext, i, gen.def.ContextSources[key].IntervalSeconds)
	}
	for i, binding := range gen.sensors {
		switch {
		case binding.cov != nil:
			//the evaluation grid carries the heartbeat too, exactly as it does
			//live: a heartbeat lands on the first grid instant at which the gap
			//has run
			add(binding.channel.Id, historyClassOf(binding.channel), i, binding.cov.evalSeconds)
		case binding.sourceInterval > 0:
			add(binding.channel.Id, historyClassOf(binding.channel), i, binding.sourceInterval)
			if channelPublishes(binding.channel) {
				add(binding.channel.Id+":publish", historyClassPublish, i, binding.channel.IntervalSeconds)
			}
		default:
			add(binding.channel.Id, historyClassOf(binding.channel), i, binding.channel.IntervalSeconds)
		}
	}
	return grids
}

// historyDistinctGrids refuses a set of grids two of which a checkpoint could
// not tell apart: remembering such a run would give both the tick of whichever
// was written last. Unique channel ids are not enough for that - a channel
// called "context:k" next to a context source k, or "X:publish" next to a split
// channel X, collides on the identity while the document validates.
func historyDistinctGrids(grids []historyGrid) error {
	seen := make(map[string]bool, len(grids))
	for i := range grids {
		if seen[grids[i].id] {
			return fmt.Errorf("two grids of this environment are both called %v, so a checkpoint of it could not be resumed", grids[i].id)
		}
		seen[grids[i].id] = true
	}
	return nil
}

// historyMaxTick is the largest tick of a grid of this step that due can still
// compute: baseUnix + tick*step has to stay an int64, and beyond that the
// comparison against the end of the window would wrap rather than refuse.
func historyMaxTick(baseUnix int64, step int64) int64 {
	limit := int64(math.MaxInt64)
	if baseUnix > 0 {
		limit -= baseUnix
	}
	if step > 1 {
		limit /= step
	}
	return limit
}

// historyResume puts a checkpoint back into the grids and the channels; state
// and value cache are the caller's job, as seeding is for a fresh run. A grid or
// channel the checkpoint says nothing about is an error: continuing it at tick
// zero would replay its whole window.
func historyResume(grids []historyGrid, channels []*historyChannel, shared *historyShared, baseUnix int64, resume *repo.HistoryCheckpoint) error {
	for i := range grids {
		tick, known := resume.Ticks[grids[i].id]
		if !known {
			return fmt.Errorf("the checkpoint knows no tick for %v, so it was not written for the definition of this run", grids[i].id)
		}
		//bounded so that the multiplication in due cannot overflow, rather than by
		//the step cap of a run: that cap is over every grid of the environment and
		//the chase adds steps outside it, so one fine grid legitimately carries
		//more ticks than it.
		if tick < 0 || tick > historyMaxTick(baseUnix, grids[i].step) {
			return fmt.Errorf("the checkpoint carries the tick %d for %v, which is not a step of a grid of a run", tick, grids[i].id)
		}
		grids[i].tick = tick
	}
	lastError := ""
	for _, channel := range channels {
		id := channel.binding.channel.Id
		memory, known := resume.Channels[id]
		if !known {
			return fmt.Errorf("the checkpoint remembers nothing about the channel %v, so it was not written for the definition of this run", id)
		}
		if memory.PendingSet {
			channel.pending.put(memory.Pending)
		}
		channel.lastAttemptUnix = memory.LastAttemptUnix
		if len(memory.Held) > 0 {
			channel.faultMemory.held = make(map[int]frozenHold, len(memory.Held))
			for _, hold := range memory.Held {
				channel.faultMemory.held[hold.Index] = frozenHold{beginUnix: hold.BeginUnix, value: hold.Value}
			}
		}
		//the counters are the totals of the whole run, so the three of them keep
		//adding up to the steps it has taken over both halves. Written under the
		//run's mutex, which is the discipline of these fields wherever they are
		//touched.
		shared.mux.Lock()
		channel.result.Published = memory.Published
		channel.result.Silent = memory.Silent
		channel.result.Failed = memory.Failed
		channel.result.LastError = memory.LastError
		shared.mux.Unlock()
		if memory.LastError != "" {
			lastError = memory.LastError
		}
	}
	//no message of its own is stored for the run, so the last one in document
	//order stands for it: a resumed run that reported a refusal must not report
	//"nothing went wrong" at its next boundary while the counters still say it did
	shared.mux.Lock()
	shared.lastError = lastError
	shared.mux.Unlock()
	return nil
}

// historyCheckpointAt is where the run stands, for a caller that has drained the
// pool and settled every ack: only then are the ticks, the channel memory and the
// state one instant.
//
// The store is called with none of these mutexes held, which is why the whole
// checkpoint is built here rather than read by the caller of the write.
func (this *Runtime) historyCheckpointAt(env *environment, grids []historyGrid, channels []*historyChannel, shared *historyShared, position time.Time) repo.HistoryJobProgress {
	memory := make(map[string]repo.HistoryChannelMemory, len(channels))
	for _, channel := range channels {
		entry := repo.HistoryChannelMemory{LastAttemptUnix: channel.lastAttemptUnix, Held: frozenHolds(channel.faultMemory)}
		if value, known := channel.pending.get(); known {
			entry.Pending, entry.PendingSet = value, true
		}
		memory[channel.binding.channel.Id] = entry
	}
	//the counters and the message belong to the pool's workers, so they are read
	//in one pass under the run's mutex: the totals and the per channel numbers of
	//one checkpoint have to be the same instant
	published, failed := int64(0), int64(0)
	shared.mux.Lock()
	for _, channel := range channels {
		entry := memory[channel.binding.channel.Id]
		entry.Published = channel.result.Published
		entry.Silent = channel.result.Silent
		entry.Failed = channel.result.Failed
		entry.LastError = channel.result.LastError
		memory[channel.binding.channel.Id] = entry
		published += channel.result.Published
		failed += channel.result.Failed
	}
	lastError := shared.lastError
	shared.mux.Unlock()

	ticks := make(map[string]int64, len(grids))
	for i := range grids {
		ticks[grids[i].id] = grids[i].tick
	}

	env.mux.Lock()
	state := env.snapshot()
	lastValues := make(map[string]float64, len(env.lastValues))
	for id, value := range env.lastValues {
		lastValues[id] = value
	}
	env.mux.Unlock()

	return repo.HistoryJobProgress{
		Position:  position,
		Published: published,
		Failed:    failed,
		LastError: lastError,
		Checkpoint: repo.HistoryCheckpoint{
			Position:   position,
			Ticks:      ticks,
			Channels:   memory,
			LastValues: lastValues,
			State:      state,
		},
	}
}

// historyAbortedAt is what an abort or a shutdown remembers: not a boundary, so
// AtBoundary stays false. An abort drops what the pool still held staged while
// their grids have moved on, so a run that dropped any is remembered without a
// checkpoint of its own and the resume replays the last chunk instead of losing
// them.
func (this *Runtime) historyAbortedAt(env *environment, grids []historyGrid, channels []*historyChannel, shared *historyShared, position time.Time) repo.HistoryJobProgress {
	progress := this.historyCheckpointAt(env, grids, channels, shared, position)
	dropped := shared.droppedCount()
	if dropped == 0 {
		return progress
	}
	util.Logger.Warn("the abort dropped readings this run had staged, so it is remembered at its last chunk boundary instead",
		"environment", env.id, "dropped", dropped, "at", position)
	//a progress whose checkpoint carries no instant leaves the stored one alone,
	//which is the rule the store states
	progress.Checkpoint = repo.HistoryCheckpoint{}
	return progress
}

// frozenHolds is what the freezes of one channel hold, in the order of the faults
// in the document: two checkpoints of the same instant have to be the same
// document.
func frozenHolds(run *faultRun) []repo.FrozenHold {
	if run == nil || len(run.held) == 0 {
		return nil
	}
	indexes := make([]int, 0, len(run.held))
	for index := range run.held {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	result := make([]repo.FrozenHold, 0, len(indexes))
	for _, index := range indexes {
		hold := run.held[index]
		result = append(result, repo.FrozenHold{Index: index, BeginUnix: hold.beginUnix, Value: hold.value})
	}
	return result
}

// historyChannelDue runs one channel at one virtual instant, in whichever of the
// three live shapes it has.
func (this *Runtime) historyChannelDue(env *environment, gen *generation, channel *historyChannel, at time.Time) {
	if channel.binding.cov != nil {
		this.historyEvaluateChange(env, gen, channel, at)
		return
	}
	if channel.binding.sourceInterval > 0 {
		//the source half of a split channel: it evolves the state and hands its
		//value to pending, and the publish half sends what is there
		this.dispatch(env, gen, channel.binding, nil, channel.pending.put, true, at)
		return
	}
	step := &historyStep{channel: channel}
	this.dispatch(env, gen, channel.binding, nil, func(value interface{}) {
		//env.mux is held here: every executor calls send inside its own run under
		//it, which is the same precondition covGate states. Ahead of the submit,
		//so a suppressed reading leaves attempted at 0 and is booked as silent
		//rather than as failed.
		value, send := this.faulted(env, channel.binding, channel.faultMemory, value, at)
		if !send {
			return
		}
		this.historySubmit(env, channel, value, at, step)
	}, true, at)
	step.seal()
}

// historyPublishDue is the publish half of a split channel. Nothing has been
// computed before the first source step, and skipping is right rather than
// sending a fabricated zero, exactly as the live runner does.
func (this *Runtime) historyPublishDue(env *environment, channel *historyChannel, at time.Time) {
	step := &historyStep{channel: channel}
	if value, known := channel.pending.get(); known {
		switch {
		case len(channel.binding.faults.list) == 0:
			//a channel without faults publishes as it always did, without the
			//mutex this branch does not otherwise need
			this.historySubmit(env, channel, value, at, step)
		default:
			//this branch runs outside a dispatch, so it takes the environment mutex
			//itself, the way the heartbeat branch below does; faulted reads and
			//writes the persisted meter offsets
			env.mux.Lock()
			reading, send := this.faulted(env, channel.binding, channel.faultMemory, value, at)
			env.mux.Unlock()
			if send {
				this.historySubmit(env, channel, reading, at, step)
			}
		}
	}
	step.seal()
}

// historyEvaluateChange is runChangeChannel on the grid: the evaluation decides
// through the gate, and the heartbeat is the condition that the gap since the
// last attempt has run.
//
// The publish goes through the pool, and its answer is collected before the next
// decision of this channel rather than at the send - which is what keeps the
// series identical to a synchronous one, since the base and the gap are settled
// before anything reads them.
//
// covDecide is called inside the send callback, which every executor invokes
// with the environment mutex held; the heartbeat branch runs outside a dispatch
// and takes it itself, as the live heartbeat case does.
func (this *Runtime) historyEvaluateChange(env *environment, gen *generation, channel *historyChannel, at time.Time) {
	//the publish of the previous step may still be in flight, and its base and
	//its gap are what this evaluation reads
	this.historySettleChange(env, channel, false)

	step := &historyStep{channel: channel}
	changeSent := false
	attempted := false
	this.dispatch(env, gen, channel.binding, nil, func(value interface{}) {
		channel.pending.put(value)
		//a second send of the same run compares against what the first left, so
		//that answer is collected before this one is decided. The one place a run
		//waits for an ack with the environment mutex held, and only for a script
		//that sends more than once per evaluation.
		if this.historySettleChange(env, channel, true) {
			changeSent = true
		}
		reading, number, numeric, ok := this.covDecide(env, channel.binding, channel.faultMemory, value, false, at)
		if !ok {
			return
		}
		attempted = true
		this.historySubmitChange(env, channel, reading, number, numeric, at, step, false)
	}, true, at)

	overdue := at.Unix()-channel.lastAttemptUnix >= channel.heartbeatSeconds
	if attempted && overdue {
		//the heartbeat of this instant depends on whether the change publish went
		//out, so exactly that one answer is waited for - and only here, where the
		//gap has run
		if this.historySettleChange(env, channel, false) {
			changeSent = true
		}
		overdue = at.Unix()-channel.lastAttemptUnix >= channel.heartbeatSeconds
	}
	if changeSent || !overdue {
		//a publish restarts the gap, so this instant owes no heartbeat
		step.seal()
		return
	}
	//nothing computed yet means there is no reading to repeat: the gap is left
	//standing, so the next evaluation is owed the heartbeat instead
	if value, known := channel.pending.get(); known {
		env.mux.Lock()
		reading, number, numeric, ok := this.covDecide(env, channel.binding, channel.faultMemory, value, true, at)
		env.mux.Unlock()
		if ok {
			this.historySubmitChange(env, channel, reading, number, numeric, at, step, true)
		}
		//restarted on the attempt whether or not it went out, mirroring the live
		//runner: keeping the old moment would make every following instant
		//overdue and shift the grid off the live cadence
		channel.lastAttemptUnix = at.Unix()
	}
	step.seal()
}

// historySettleChange collects the answer of the publish that may still be in
// flight and applies what depended on it - the comparison base a sent reading
// leaves, and the gap a sent change publish restarts - reporting whether a change
// publish went out. held says whether the caller already owns the environment
// mutex, which the call inside a dispatch does and the one before it does not.
func (this *Runtime) historySettleChange(env *environment, channel *historyChannel, held bool) bool {
	outstanding := channel.outstanding
	if outstanding == nil {
		return false
	}
	channel.outstanding = nil
	if sent := <-channel.acks; !sent {
		return false
	}
	if !held {
		env.mux.Lock()
		defer env.mux.Unlock()
	}
	covBook(env, channel.binding, outstanding.number, outstanding.numeric, outstanding.at)
	if outstanding.forced {
		//the heartbeat restarted the gap on the attempt, in the evaluation itself
		return false
	}
	channel.lastAttemptUnix = outstanding.at.Unix()
	return true
}

// historySettleAll collects every answer still open, for a caller that has
// drained the pool: the base of the last publish of a channel has to be in the
// state before the environment is handed back to the live simulation.
func (this *Runtime) historySettleAll(env *environment, channels []*historyChannel) {
	for _, channel := range channels {
		this.historySettleChange(env, channel, false)
	}
}

// historySubmitChange hands one reading of a channel with a change trigger to
// the pool and remembers what its answer will decide. The caller has settled the
// previous one, so the one slot is free.
func (this *Runtime) historySubmitChange(env *environment, channel *historyChannel, reading interface{}, number float64, numeric bool, at time.Time, step *historyStep, forced bool) {
	if !channel.result.Publishable {
		return
	}
	step.attempt()
	channel.outstanding = &covOutstanding{at: at, number: number, numeric: numeric, forced: forced}
	channel.shared.pool.Submit(publishJob{
		channelId: channel.binding.channel.Id,
		binding:   channel.binding,
		value:     reading,
		at:        at,
		done: func(sent bool, err error) {
			this.historyPublished(env, channel, sent, err, at)
			step.ack(sent, errors.Is(err, ErrPublishAborted))
			channel.acks <- sent
		},
	})
}

// historySubmit hands one reading to the pool without waiting for it. A channel
// that cannot publish attempts nothing, which is what leaves its first live
// value to go out unconditionally.
//
// It needs no lock of its own and is called both with the environment mutex held
// (from inside a dispatch) and without it (the publish half of a split channel).
// The workers and the callback below may never take that mutex.
func (this *Runtime) historySubmit(env *environment, channel *historyChannel, value interface{}, at time.Time, step *historyStep) {
	if !channel.result.Publishable {
		return
	}
	step.attempt()
	channel.shared.pool.Submit(publishJob{
		channelId: channel.binding.channel.Id,
		binding:   channel.binding,
		value:     value,
		at:        at,
		done: func(sent bool, err error) {
			this.historyPublished(env, channel, sent, err, at)
			step.ack(sent, errors.Is(err, ErrPublishAborted))
		},
	})
}

// historyPublished takes what became of one attempt. It runs on the pool's
// workers, so everything it touches is under the run's mutex and the last error
// is the most recently acked one rather than the latest by instant.
//
// An abort stands as the reason only while no platform error is known, and is not
// logged: the state of the run already says why its last readings are missing.
func (this *Runtime) historyPublished(env *environment, channel *historyChannel, sent bool, err error, at time.Time) {
	if sent {
		return
	}
	aborted := errors.Is(err, ErrPublishAborted)
	channel.shared.mux.Lock()
	if aborted {
		//counted, because a checkpoint at the abort position would claim the step
		//of this reading was covered; see historyAbortedAt
		channel.shared.dropped++
	}
	if err != nil {
		if !aborted || channel.result.LastError == "" {
			channel.result.LastError = err.Error()
		}
		if !aborted || channel.shared.lastError == "" {
			channel.shared.lastError = err.Error()
		}
	}
	report := !aborted && !channel.reported
	if report {
		channel.reported = true
	}
	channel.shared.mux.Unlock()
	if report {
		util.Logger.Warn("unable to publish a reading of the history run", attributes.ErrorKey, err,
			"environment", env.id, "channel", channel.binding.channel.Id, "at", at)
	}
}

// historyChannelShape answers, once per channel, whether a reading of it can
// reach timescale under a past timestamp, and with which shape. err is set only
// where the lookup itself failed - a device repository that could not be read
// says nothing about the channel, unlike a service that carries no time path -
// so a caller that can still refuse the run may act on it.
func (this *Runtime) historyChannelShape(binding channelBinding) (devices.TimeShape, string, bool, error) {
	if !channelPublishes(binding.channel) {
		return devices.TimeShape{}, "the channel does not publish on a schedule, so it only computes", false, nil
	}
	if binding.asset.externalRef == "" {
		return devices.TimeShape{}, "the asset has no platform device, so a reading has nowhere to go", false, nil
	}
	if binding.channel.ExternalRef == "" {
		return devices.TimeShape{}, "the channel has no platform service, so a reading has nowhere to go", false, nil
	}
	shape, err := this.publisher.TimeShapeOf(binding.asset.externalRef, binding.channel.ExternalRef)
	if errors.Is(err, devices.ErrNoTimePath) || errors.Is(err, devices.ErrUnusableTimeShape) {
		//a property of the service, not a lookup that failed: such a channel can
		//never publish with a timestamp, so it only computes and cannot occupy
		return devices.TimeShape{}, err.Error(), false, nil
	}
	if err != nil {
		return devices.TimeShape{}, err.Error(), false, err
	}
	return shape, "", true, nil
}

// historyPublishable is the same question where nothing can be refused any
// more: the run is under way, so a lookup that failed leaves the channel
// computing without publishing, with the reason it gave.
func (this *Runtime) historyPublishable(binding channelBinding) (bool, string) {
	_, reason, publishable, _ := this.historyChannelShape(binding)
	return publishable, reason
}

// channelPublishes is the generation's own test for "this channel sends on its
// interval", asked again where only the channel is at hand.
func channelPublishes(channel domain.Channel) bool {
	return channel.Direction == domain.Sensor &&
		channel.IntervalSeconds > 0 && channel.IntervalSeconds <= maxIntervalSeconds
}
