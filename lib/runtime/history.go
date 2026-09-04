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
	"math"
	"sort"
	"sync"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/domain"
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

// historyEngineFunc is the seam between the run and the lifecycle around it.
type historyEngineFunc func(ctx context.Context, env *environment, gen *generation, from time.Time, to time.Time, chase bool, progress historyProgress) (HistoryResult, error)

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
type historyGrid struct {
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
func (this *Runtime) runHistory(ctx context.Context, env *environment, gen *generation, from time.Time, to time.Time, chase bool, progress historyProgress) (HistoryResult, error) {
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

	keys := make([]string, 0, len(gen.def.ContextSources))
	for key := range gen.def.ContextSources {
		keys = append(keys, key)
	}
	//sorted, so that two context sources due at the same instant move in the same
	//order on every run
	sort.Strings(keys)

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
	grids := []historyGrid{}
	addGrid := func(class int, order int, step int64) {
		grids = append(grids, historyGrid{class: class, order: order, step: step})
	}

	for i, key := range keys {
		addGrid(historyClassContext, i, gen.def.ContextSources[key].IntervalSeconds)
	}
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
		switch {
		case binding.cov != nil:
			//the evaluation grid carries the heartbeat too, exactly as it does
			//live: a heartbeat lands on the first grid instant at which the gap
			//has run
			addGrid(historyClassOf(binding.channel), i, binding.cov.evalSeconds)
		case binding.sourceInterval > 0:
			addGrid(historyClassOf(binding.channel), i, binding.sourceInterval)
			if channelPublishes(binding.channel) {
				addGrid(historyClassPublish, i, binding.channel.IntervalSeconds)
			}
		default:
			addGrid(historyClassOf(binding.channel), i, binding.channel.IntervalSeconds)
		}
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

	position := from
	processed := int64(0)
	//lastGap is what the previous pass had left to close; see historyChasesOn
	lastGap := time.Duration(math.MaxInt64)

	for round := 0; ; round++ {
		for queue.Len() > 0 {
			if err := ctx.Err(); err != nil {
				//booked before the counters are read, or the three of them would
				//not add up to the steps the run took
				pool.Drain()
				this.historySettleAll(env, channels)
				result.Channels = historyResults(channels, shared)
				result.Published, result.Failed, result.LastError = historyTotals(channels, shared)
				//the end of an aborted run is the last instant it actually
				//simulated: a chase round that was cut short had already raised
				//end, and reporting that would claim a span the run never covered
				result.Position, result.End = position, position
				return result, err
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
		pool.Drain()
		this.historySettleAll(env, channels)

		//the window is drained; a long run has meanwhile lost the time it spent
		//simulating, and handing the environment over across that hole would put
		//the step back that the mode exists to avoid
		if !chase || round >= historyCatchUpRounds {
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

// historyPublishable answers, once per channel and before the first instant,
// whether a reading of it can reach timescale under a past timestamp. The last
// question costs a device type read, which is why it is not asked per instant.
func (this *Runtime) historyPublishable(binding channelBinding) (bool, string) {
	if !channelPublishes(binding.channel) {
		return false, "the channel does not publish on a schedule, so it only computes"
	}
	if binding.asset.externalRef == "" {
		return false, "the asset has no platform device, so a reading has nowhere to go"
	}
	if binding.channel.ExternalRef == "" {
		return false, "the channel has no platform service, so a reading has nowhere to go"
	}
	if _, err := this.publisher.TimeShapeOf(binding.asset.externalRef, binding.channel.ExternalRef); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// channelPublishes is the generation's own test for "this channel sends on its
// interval", asked again where only the channel is at hand.
func channelPublishes(channel domain.Channel) bool {
	return channel.Direction == domain.Sensor &&
		channel.IntervalSeconds > 0 && channel.IntervalSeconds <= maxIntervalSeconds
}
