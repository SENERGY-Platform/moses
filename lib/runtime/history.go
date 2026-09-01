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
	"math"
	"sort"
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

// historyShared is what the whole run keeps rather than one channel: the message
// of the most recent publish failure. Recent by instant, since the events are
// processed in the order of the virtual clock.
type historyShared struct {
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

	// reported keeps the log to one line per broken channel.
	reported bool

	// shared carries what belongs to the run rather than to this channel.
	shared *historyShared

	result HistoryChannelStatus
}

// historyOutcome is what one step of one channel did, before it is folded into
// the three counters.
type historyOutcome struct {
	attempted int
	published int
}

func (this *historyChannel) record(outcome historyOutcome) {
	switch {
	case outcome.attempted == 0:
		this.result.Silent++
	case outcome.published > 0:
		this.result.Published++
	default:
		this.result.Failed++
	}
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

	channels := make([]*historyChannel, len(gen.sensors))
	shared := &historyShared{}
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
				result.Channels = historyResults(channels)
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

	result.Channels = historyResults(channels)
	result.Published, result.Failed, result.LastError = historyTotals(channels, shared)
	result.Position, result.End = position, end
	if progress != nil {
		progress(position, result.Published, result.Failed, result.LastError)
	}
	return result, nil
}

func historyResults(channels []*historyChannel) []HistoryChannelStatus {
	result := make([]HistoryChannelStatus, 0, len(channels))
	for _, channel := range channels {
		result = append(result, channel.result)
	}
	return result
}

func historyTotals(channels []*historyChannel, shared *historyShared) (published int64, failed int64, lastError string) {
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
	outcome := historyOutcome{}
	this.dispatch(env, gen, channel.binding, nil, func(value interface{}) {
		//env.mux is held here: every executor calls send inside its own run under
		//it, which is the same precondition covGate states. Ahead of historySend,
		//so a suppressed reading leaves attempted at 0 and is booked as silent
		//rather than as failed.
		value, send := this.faulted(env, channel.binding, channel.faultMemory, value, at)
		if !send {
			return
		}
		this.historySend(env, channel, value, at, &outcome)
	}, true, at)
	channel.record(outcome)
}

// historyPublishDue is the publish half of a split channel. Nothing has been
// computed before the first source step, and skipping is right rather than
// sending a fabricated zero, exactly as the live runner does.
func (this *Runtime) historyPublishDue(env *environment, channel *historyChannel, at time.Time) {
	outcome := historyOutcome{}
	if value, known := channel.pending.get(); known {
		switch {
		case len(channel.binding.faults.list) == 0:
			//a channel without faults publishes as it always did, without the
			//mutex this branch does not otherwise need
			this.historySend(env, channel, value, at, &outcome)
		default:
			//this branch runs outside a dispatch, so it takes the environment mutex
			//itself, the way the heartbeat branch below does; faulted reads and
			//writes the persisted meter offsets
			env.mux.Lock()
			reading, send := this.faulted(env, channel.binding, channel.faultMemory, value, at)
			env.mux.Unlock()
			if send {
				this.historySend(env, channel, reading, at, &outcome)
			}
		}
	}
	channel.record(outcome)
}

// historyEvaluateChange is runChangeChannel on the grid: the evaluation decides
// through the gate, and the heartbeat is the condition that the gap since the
// last attempt has run.
//
// The gate is called inside the send callback, which every executor invokes with
// the environment mutex held; taking that mutex here would deadlock. The
// heartbeat branch runs outside a dispatch and therefore takes it itself, the
// same way the live heartbeat case does.
func (this *Runtime) historyEvaluateChange(env *environment, gen *generation, channel *historyChannel, at time.Time) {
	outcome := historyOutcome{}
	send := func(value interface{}) bool {
		return this.historySend(env, channel, value, at, &outcome)
	}
	this.dispatch(env, gen, channel.binding, nil, func(value interface{}) {
		channel.pending.put(value)
		this.covGate(env, channel.binding, channel.faultMemory, value, false, at, send)
	}, true, at)

	if outcome.published > 0 {
		//a publish restarts the gap, so this instant owes no heartbeat
		channel.lastAttemptUnix = at.Unix()
		channel.record(outcome)
		return
	}
	if at.Unix()-channel.lastAttemptUnix >= channel.heartbeatSeconds {
		//nothing computed yet means there is no reading to repeat: the gap is left
		//standing, so the next evaluation is owed the heartbeat instead
		if value, known := channel.pending.get(); known {
			env.mux.Lock()
			this.covGate(env, channel.binding, channel.faultMemory, value, true, at, send)
			env.mux.Unlock()
			//restarted on the attempt whether or not it went out, mirroring the
			//live runner: keeping the old moment would make every following
			//instant overdue and shift the grid off the live cadence
			channel.lastAttemptUnix = at.Unix()
		}
	}
	channel.record(outcome)
}

// historySend publishes one reading under its virtual instant. A channel that
// cannot publish attempts nothing, so it never books a comparison base either -
// which is what leaves its first live value to go out unconditionally.
func (this *Runtime) historySend(env *environment, channel *historyChannel, value interface{}, at time.Time, outcome *historyOutcome) bool {
	if !channel.result.Publishable {
		return false
	}
	outcome.attempted++
	//report false: the failure is counted and named in the status, and one line
	//per lost reading would bury the service log over a window of a year
	sent, err := this.publishAt(env, channel.binding, value, false, at)
	if sent {
		outcome.published++
		return true
	}
	if err != nil {
		channel.result.LastError = err.Error()
		//the events run in the order of the virtual clock, so the last write here
		//is the most recent failure of the run and not merely the last channel's
		channel.shared.lastError = err.Error()
	}
	if !channel.reported {
		channel.reported = true
		util.Logger.Warn("unable to publish a reading of the history run", attributes.ErrorKey, err,
			"environment", env.id, "channel", channel.binding.channel.Id, "at", at)
	}
	return false
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
