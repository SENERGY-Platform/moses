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
	"fmt"
	"math"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// Change of value publishing: a channel sends when its value moves and,
// independently of that, at least once per heartbeat - the way real metering
// hardware behaves, and what a ticker-only simulation cannot reproduce.
//
// Three pieces split by what they need: exceedsChange and covSends are pure
// and shared by the live path and the backfill, which is the parity
// guarantee; covPublish is the gate and runs under the environment mutex;
// runChangeChannel owns the two timers and is the only goroutine touching
// them.

// covSettings is a resolved change trigger: the thresholds plus the cadence the
// value is computed and compared on.
type covSettings struct {
	absolute    float64
	relative    float64
	evalSeconds int64
}

// covOf resolves what publish_on_change means for one channel.
//
// The third result is empty when the trigger is usable or absent, and otherwise
// says why it cannot be used - so the generation can warn about a document that
// bypassed validation while the backfill, which walks the same definition, can
// ask the same question without logging anything. Both have to reach the same
// answer: a backfill counting evaluation steps for a trigger the live runtime
// degraded would refuse a window the job could serve.
func covOf(channel domain.Channel) (covSettings, bool, string) {
	trigger := channel.PublishOnChange
	if trigger == nil {
		return covSettings{}, false, ""
	}
	if channel.Direction != domain.Sensor {
		return covSettings{}, false, "only a sensor publishes readings of its own"
	}
	if channel.IntervalSeconds <= 0 {
		return covSettings{}, false, "the channel has no interval to use as its heartbeat"
	}
	if channel.IntervalSeconds > maxIntervalSeconds {
		return covSettings{}, false, "the channel interval is out of range"
	}
	//NaN and infinity are refused by validation and would disable the threshold
	//silently here: NaN is never greater than anything, infinity is never
	//exceeded. Both are caught by the "greater than zero" test below for NaN and
	//named explicitly for infinity.
	absolute, relative := trigger.Absolute, trigger.Relative
	if math.IsInf(absolute, 0) || math.IsInf(relative, 0) {
		return covSettings{}, false, "a threshold is infinite and could never be exceeded"
	}
	if !(absolute > 0) && !(relative > 0) {
		return covSettings{}, false, "neither an absolute nor a relative threshold is set, so nothing would ever count as a change"
	}
	//exactly one cadence, the way validation demands it. Both set is refused
	//rather than resolved: running the source on the evaluation cadence would
	//silently change how often the simulation itself advances.
	if trigger.EvaluateIntervalSeconds > 0 && channel.Source.IntervalSeconds > 0 {
		return covSettings{}, false, "the channel carries both a source interval and an evaluation interval, and there can only be one evaluation cadence"
	}
	evalSeconds := trigger.EvaluateIntervalSeconds
	if evalSeconds <= 0 {
		evalSeconds = channel.Source.IntervalSeconds
	}
	if evalSeconds <= 0 {
		return covSettings{}, false, "the value is never computed between two heartbeats, so a change could not be noticed"
	}
	if evalSeconds > maxIntervalSeconds {
		return covSettings{}, false, "the evaluation interval is out of range"
	}
	if evalSeconds > channel.IntervalSeconds {
		return covSettings{}, false, fmt.Sprintf(
			"the value would be computed every %d seconds while the heartbeat fires every %d, so the heartbeat would always be first",
			evalSeconds, channel.IntervalSeconds)
	}
	return covSettings{absolute: absolute, relative: relative, evalSeconds: evalSeconds}, true, ""
}

// finite is what "a value one can measure a change against" means here. Only a
// finite number has a distance to another one: the distance to NaN is NaN and
// the distance to an infinity is infinite, and neither is a movement of the
// quantity the channel measures.
func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// exceedsChange is the threshold arithmetic: does current differ from the last
// published value by more than one of the thresholds.
//
// Three properties are deliberate: the thresholds are ORed, since a document
// may carry both an absolute and a relative one; the relative threshold
// multiplies rather than divides, so at last == 0 every deviation counts as a
// change instead of dividing by zero; and a value that is not finite is never
// a change - NaN falls out of the comparison on its own, but an infinity does
// not (|±Inf - last| exceeds every finite threshold), so this is checked
// explicitly.
func exceedsChange(cov covSettings, last float64, current float64) bool {
	if !finite(current) || !finite(last) {
		return false
	}
	delta := math.Abs(current - last)
	if cov.absolute > 0 && delta > cov.absolute {
		return true
	}
	if cov.relative > 0 && delta > cov.relative*math.Abs(last) {
		return true
	}
	return false
}

// covSends is the whole "should the trigger send this" decision, shared
// structurally by the live gate and the backfill, the way exceedsChange is.
//
// base is nil when nothing has been published yet, and the first value then
// goes out without a comparison so a fresh environment does not stay silent
// until its value happens to move. That bypass does not extend to a
// non-finite value: without the exception a channel whose first value is NaN
// would publish on every evaluation forever, since exceedsChange is never
// asked about it; such a value instead waits for the heartbeat.
func covSends(cov covSettings, base *float64, current float64) bool {
	if base == nil {
		return finite(current)
	}
	return exceedsChange(cov, *base, current)
}

// covLogGate throttles the failure output of one channel: a refused publish is
// still retried on the evaluation cadence, but only the first attempt's log
// line is kept, so one broken channel does not bury the service log in
// repeated ERROR lines.
//
// No lock, and none is needed: the only writer is runChangeChannel's own
// goroutine, since its two branches are cases of one select and dispatch runs
// the source synchronously on that same goroutine.
type covLogGate struct {
	// failing is true while the last attempt did not reach the platform. The
	// first failure after a success reports, the ones after it stay quiet, and a
	// success re-arms the report - so a channel that breaks again is heard again.
	failing bool
}

// covSend publishes and keeps the log gate. Everything the trigger sends goes
// through here rather than through publish directly.
func (this *Runtime) covSend(env *environment, binding channelBinding, value interface{}, logs *covLogGate) bool {
	sent := this.publishReporting(env, binding, value, !logs.failing)
	logs.failing = !sent
	return sent
}

// covPublish is the gate in front of publish. It reports whether something went
// out, which is what restarts the heartbeat gap.
//
// It must be called with env.mux held, like every other publish path: reading
// the last published value, sending, and writing the new one back have to be
// one operation, or two interleaved evaluations could compare against a value
// that was never published, or lose one that was.
//
// forced is the heartbeat: it skips the threshold, not the bookkeeping. A
// failed publish never advances the bookkeeping, so the next evaluation still
// sees the old comparison base and retries without a queue to maintain.
func (this *Runtime) covPublish(env *environment, binding channelBinding, value interface{}, forced bool, logs *covLogGate) bool {
	number, numeric := asFloat(value)
	if !numeric {
		//fail open: a channel whose script sends a string or a boolean has no
		//distance between two values, and dropping such a value would silence
		//the channel entirely. It publishes on every evaluation instead, and the
		//bookkeeping is left alone so a later numeric value still compares
		//against the last number that actually went out.
		return this.covSend(env, binding, value, logs)
	}
	if !forced {
		var base *float64
		if last, known := env.state.LastPublished[binding.channel.Id]; known {
			//a copy out of the map, so the address below is the local one
			stored := last.Value
			base = &stored
		}
		if !covSends(*binding.cov, base, number) {
			return false
		}
	}
	if !this.covSend(env, binding, value, logs) {
		return false
	}
	if !finite(number) {
		//it went out, so the heartbeat gap restarts, but it must not become the
		//comparison base: every later comparison against it would be false and
		//the channel would fall back to the heartbeat forever. The stored state
		//cannot hold it either (copyValue turns it into 0, which would be a
		//comparison base nobody wrote).
		return true
	}
	if env.state.LastPublished == nil {
		env.state.LastPublished = map[string]repo.PublishedValue{}
	}
	env.state.LastPublished[binding.channel.Id] = repo.PublishedValue{Value: number, AtUnix: time.Now().Unix()}
	env.dirty = true
	return true
}

// runChangeChannel drives one channel that publishes on change: an evaluation
// ticker that computes the value and lets the gate decide, and a heartbeat timer
// that sends whatever was computed last when the gate has been quiet for too
// long.
//
// Both timers belong to this goroutine alone. The reset after a publish is done
// here rather than in the send closure because a script may call send zero or
// several times in one run: every one of those values goes through the gate on
// its own, and the heartbeat is reset once, after the run.
//
// pending keeps the last computed value rather than consuming it, like the
// split channel does: the heartbeat means "this is still the reading", so it
// repeats the value it last saw instead of skipping.
func (this *Runtime) runChangeChannel(ctx context.Context, env *environment, gen *generation, binding channelBinding) {
	heartbeatEvery := time.Duration(binding.channel.IntervalSeconds) * time.Second
	evalEvery := time.Duration(binding.cov.evalSeconds) * time.Second

	evaluate := time.NewTicker(evalEvery)
	defer evaluate.Stop()
	heartbeat := time.NewTimer(this.firstHeartbeatDelay(env, binding, heartbeatEvery, evalEvery))
	defer heartbeat.Stop()
	pending := &latest{}
	logs := &covLogGate{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-evaluate.C:
			if this.evaluateChangeChannel(env, gen, binding, pending, logs) {
				heartbeat.Reset(heartbeatEvery)
			}
		case <-heartbeat.C:
			//an evaluation that is due at this very instant is served first: a
			//select picks at random between two ready cases, and a heartbeat
			//interval that is a whole multiple of the evaluation cadence - the
			//shape every document has - makes both due at the same moment on every
			//heartbeat. Taking the heartbeat first would publish the current or the
			//previous value on a coin toss.
			if this.dueEvaluation(evaluate, env, gen, binding, pending, logs) {
				//that evaluation published, so the gap starts again and this
				//heartbeat is not owed any more: sending the same value a second
				//time in the same instant is the duplicate reading the reset
				//after a change publish exists to avoid
				heartbeat.Reset(heartbeatEvery)
				continue
			}
			value, known := pending.get()
			if !known {
				//nothing has been computed yet, so there is no reading to
				//repeat. Waiting one evaluation rather than one heartbeat keeps
				//the gap measured from the first value instead of from a
				//heartbeat that sent nothing, and cannot spin: an evaluation
				//interval is at least one second.
				heartbeat.Reset(evalEvery)
				continue
			}
			env.mux.Lock()
			this.covPublish(env, binding, value, true, logs)
			env.mux.Unlock()
			//reset whether or not it went out: a failed publish must not end the
			//heartbeat for good, and the next one is a full gap away rather than
			//immediately, so a broken connector is retried and not hammered
			heartbeat.Reset(heartbeatEvery)
		}
	}
}

// evaluateChangeChannel computes the channel once and lets the gate decide over
// every value the run produced. It reports whether anything went out, which is
// what restarts the heartbeat gap.
//
// A script may call send zero or several times in one run: each of those values
// goes through the gate on its own, and the heartbeat is reset once, by the
// caller, after the run.
func (this *Runtime) evaluateChangeChannel(env *environment, gen *generation, binding channelBinding, pending *latest, logs *covLogGate) bool {
	published := false
	this.dispatch(env, gen, binding, nil, func(value interface{}) {
		pending.put(value)
		if this.covPublish(env, binding, value, false, logs) {
			published = true
		}
	}, true)
	return published
}

// dueEvaluation runs the evaluation that is already waiting in the ticker, if
// there is one, and reports whether it published. It never blocks: with nothing
// waiting it does nothing at all, which is the case of a heartbeat that falls
// between two evaluations.
func (this *Runtime) dueEvaluation(evaluate *time.Ticker, env *environment, gen *generation, binding channelBinding, pending *latest, logs *covLogGate) bool {
	select {
	case <-evaluate.C:
		return this.evaluateChangeChannel(env, gen, binding, pending, logs)
	default:
		return false
	}
}

// firstHeartbeatDelay is how long after a start the first heartbeat may be. A
// restart is not the start of the gap: what was published before the restart
// still stands, so only the rest of that gap is owed.
//
// Clamped on both sides: below by one evaluation, so a gap that has long run
// out does not fire before anything has been computed; above by the full gap,
// against a clock that jumped backwards or a stored timestamp from the
// future.
func (this *Runtime) firstHeartbeatDelay(env *environment, binding channelBinding, heartbeatEvery time.Duration, evalEvery time.Duration) time.Duration {
	env.mux.Lock()
	last, known := env.state.LastPublished[binding.channel.Id]
	env.mux.Unlock()
	if !known {
		return heartbeatEvery
	}
	return min(max(heartbeatEvery-time.Since(time.Unix(last.AtUnix, 0)), evalEvery), heartbeatEvery)
}
