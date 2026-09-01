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
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

// A fault sits in the measurement, not in the world: it is applied after the
// value has been computed and remembered, so formulas, aggregates, the asset
// counter and the value cache all keep the undisturbed reading and only what
// leaves for the platform is disturbed. That is what makes the ground truth
// available. See docs/injected-faults.md.
//
// Everything below is a pure function of the seed, the channel id and the
// instant, apart from two pieces of memory that cannot be: the value a freeze
// holds (faultRun, run local) and the offset a meter exchange captured
// (RuntimeState.MeterExchanges, persisted).

// resolvedFault is one fault of one channel, resolved against the channel's
// evaluation step. index is its position in the document and is part of every
// draw and every stored key, so two faults of one channel never share either.
type resolvedFault struct {
	kind  domain.FaultKind
	index int

	// windowed says which of the two triggers below applies.
	windowed bool
	fromUnix int64
	// toUnix is exclusive, and meaningless for a meter exchange.
	toUnix int64

	// probability is the chance that an occurrence begins in one step, and
	// lookbackSlots is how many steps back a running one can still have begun.
	probability     float64
	durationSeconds int64
	lookbackSlots   int64

	factor  float64
	resetTo float64

	stepSeconds int64
}

// channelFaults is a channel's usable faults bound to what drawing an occurrence
// needs. The zero value is the ordinary case - a document without faults - and
// every caller short circuits on an empty list, which is what keeps such a
// document byte identical to what it produced before the field existed.
type channelFaults struct {
	seed      int64
	channelId string
	list      []resolvedFault

	// exchanges says whether any fault writes a meter offset, so a caller knows
	// whether it has to hand in a map at all.
	exchanges bool
}

// frozenHold is the value one freeze holds, together with the instant its
// occurrence began: a second occurrence of the same fault begins at another
// instant and therefore takes its own value rather than inheriting this one.
type frozenHold struct {
	beginUnix int64
	value     float64
}

// faultRun is the memory the faults of one running channel keep, for the
// lifetime of one runner, one history channel or one backfill loop. It is
// deliberately not persisted: a restart in the middle of a freeze takes the held
// value again, which is a documented gap rather than a fourth stored map.
//
// No lock, and none is needed: a channel's faults are only ever evaluated by the
// one goroutine that drives it.
type faultRun struct {
	held map[int]frozenHold
}

// hold is the value a freeze reports at an occurrence that began at beginUnix:
// the first value it sees, and that same value from then on.
func (this *faultRun) hold(index int, beginUnix int64, value float64) float64 {
	if this == nil {
		return value
	}
	if previous, known := this.held[index]; known && previous.beginUnix == beginUnix {
		return previous.value
	}
	if this.held == nil {
		this.held = map[int]frozenHold{}
	}
	this.held[index] = frozenHold{beginUnix: beginUnix, value: value}
	return value
}

// meterExchangeKey identifies one exchange: the instant it happens at and the
// channel. Deliberately NOT the fault's position in the document - that is not a
// stable identity. Deleting an unrelated fault earlier in the list shifts every
// index behind it, the prune would then read the stored offset as belonging to
// nothing, and the published register would jump back to reset_to: a backwards
// step in a cumulative counter, which is precisely the signal "the meter was
// exchanged", raised by an edit that touched something else. The instant is
// mandatory and unique per channel for this kind (see checkFaults and faultsOf),
// so it identifies the exchange on its own.
//
// The channel id comes last on purpose: it is the only part that may contain the
// separator, so reading the key back is unambiguous whatever an id looks like.
func meterExchangeKey(channelId string, atUnix int64) string {
	return strconv.FormatInt(atUnix, 10) + "|" + channelId
}

// faultDrawKey is the identity a drawn occurrence is hashed under, so that two
// rate faults of one channel draw independently.
//
// The index comes first for the reason spreadDraw hashes the channel id first:
// fnv-1a carries a byte upwards only through the multiplications after it. The
// number bytes spreadDraw appends are enough to avalanche an index at the end of
// this key today, but a key whose discriminating part sits last is one change to
// that tail away from collapsing again - and this one collapsed completely once.
func faultDrawKey(channelId string, index int) string {
	return strconv.Itoa(index) + "|fault|" + channelId
}

// newChannelFaults resolves the faults of one channel and binds them to the draw.
func newChannelFaults(seed int64, channel domain.Channel, stepSeconds int64) (channelFaults, []string) {
	list, reasons := faultsOf(channel, stepSeconds)
	result := channelFaults{seed: seed, channelId: channel.Id, list: list}
	for i := range list {
		if list[i].kind == domain.FaultMeterExchange {
			result.exchanges = true
		}
	}
	return result, reasons
}

// faultsOf resolves what the faults of one channel mean on its evaluation step.
//
// The second result names every fault that cannot be used, the way covOf names an
// unusable trigger: the generation warns about a document that bypassed
// validation, while the backfill, which walks the same definition, asks the same
// question without logging. Both have to reach the same answer, or a reconstructed
// window would carry defects the live channel does not.
//
// stepSeconds must be the channel's evaluation step (channelBinding.stepSeconds),
// never its publish interval: the slot of a drawn occurrence is counted in it, and
// two paths counting in different units would draw different occurrences.
//
// Fields a kind does not read are not checked here although validation refuses
// them - they are simply never looked at, and dropping the whole fault over one
// would lose a defect the author did describe.
func faultsOf(channel domain.Channel, stepSeconds int64) ([]resolvedFault, []string) {
	if len(channel.Faults) == 0 {
		return nil, nil
	}
	if len(channel.Faults) > domain.MaxChannelFaults {
		return nil, []string{fmt.Sprintf("the channel carries %d faults, more than the %d a channel may have, so none of them is injected",
			len(channel.Faults), domain.MaxChannelFaults)}
	}
	if channel.Direction != domain.Sensor {
		return nil, []string{"only a sensor publishes readings of its own, so only a sensor has readings a fault could disturb"}
	}
	if stepSeconds <= 0 || stepSeconds > maxIntervalSeconds {
		return nil, []string{"the channel has no usable evaluation step, so there is no grid to place a fault on"}
	}
	list := make([]resolvedFault, 0, len(channel.Faults))
	reasons := []string{}
	//the instant is the whole identity of a stored meter offset, so a second
	//exchange of one channel at the same instant would read the first one's offset
	//and apply it twice. Validation refuses the pair; here the second one is
	//dropped, in document order, so the first keeps its instant.
	exchanged := map[int64]bool{}
	for i := range channel.Faults {
		resolved, reason := resolveFault(channel, channel.Faults[i], i, stepSeconds)
		if reason == "" && resolved.kind == domain.FaultMeterExchange {
			if exchanged[resolved.fromUnix] {
				reason = "another meter exchange of this channel already happens at that instant, and the two would share one stored offset"
			} else {
				exchanged[resolved.fromUnix] = true
			}
		}
		if reason != "" {
			reasons = append(reasons, fmt.Sprintf("faults[%d] (%s): %s", i, channel.Faults[i].Kind, reason))
			continue
		}
		list = append(list, resolved)
	}
	if len(list) == 0 {
		list = nil
	}
	return list, reasons
}

func resolveFault(channel domain.Channel, fault domain.Fault, index int, stepSeconds int64) (resolvedFault, string) {
	result := resolvedFault{kind: fault.Kind, index: index, stepSeconds: stepSeconds,
		factor: fault.Factor, resetTo: fault.ResetTo}
	switch fault.Kind {
	case domain.FaultOutage, domain.FaultFrozen, domain.FaultSpike, domain.FaultMeterExchange:
	default:
		return result, "unknown fault kind"
	}
	if fault.Kind == domain.FaultSpike && (math.IsNaN(fault.Factor) || math.IsInf(fault.Factor, 0)) {
		return result, "the factor is not a finite number, so the reading would become one nothing downstream can use"
	}
	if fault.Kind == domain.FaultMeterExchange {
		if math.IsNaN(fault.ResetTo) || math.IsInf(fault.ResetTo, 0) {
			return result, "reset_to is not a finite number, so the register would restart at a reading nothing can count from"
		}
		if !domain.CumulativeSource(channel.Source) {
			return result, "the channel's source does not count up, so there is no register to restart"
		}
	}

	windowed := !fault.From.IsZero() || !fault.To.IsZero()
	rated := fault.PerHour != 0 || fault.DurationSeconds != 0
	switch {
	case windowed && rated:
		return result, "the fault carries both a window and a rate, and which of the two decides when it occurs follows from nothing the document says"
	case !windowed && !rated:
		return result, "the fault carries neither a window nor a rate, so it never occurs"
	}

	if windowed {
		result.windowed = true
		if fault.From.IsZero() {
			return result, "the window has no start"
		}
		result.fromUnix = fault.From.Unix()
		if fault.Kind == domain.FaultMeterExchange {
			//no end by design: the new register keeps counting
			return result, ""
		}
		if fault.To.IsZero() {
			return result, "the window has no end"
		}
		result.toUnix = fault.To.Unix()
		if result.toUnix <= result.fromUnix {
			return result, "the window is empty, since its end is exclusive"
		}
		return result, ""
	}

	if fault.Kind == domain.FaultMeterExchange {
		return result, "a meter exchange happens at one instant and cannot be drawn at a rate"
	}
	if math.IsNaN(fault.PerHour) || math.IsInf(fault.PerHour, 0) || !(fault.PerHour > 0) {
		return result, "the rate is not a positive finite number, so the fault never occurs"
	}
	if fault.DurationSeconds < 1 {
		return result, "an occurrence of no length is never observed"
	}
	lookback := fault.DurationSeconds / stepSeconds
	if fault.DurationSeconds%stepSeconds != 0 {
		lookback++
	}
	if lookback > domain.MaxFaultLookbackSlots {
		return result, fmt.Sprintf("an occurrence spans %d evaluation steps, more than the %d a running one is searched back over", lookback, domain.MaxFaultLookbackSlots)
	}
	probability := fault.PerHour * float64(stepSeconds) / 3600
	if probability > 1 {
		return result, "more than one occurrence would begin per evaluation step, which a single draw per step cannot express"
	}
	result.probability = probability
	result.durationSeconds = fault.DurationSeconds
	result.lookbackSlots = lookback
	return result, ""
}

// faultBegan is the draw that decides whether an occurrence starts in one slot.
// spreadDraw hands out [-1, 1), which is mapped onto [0, 1) here rather than
// compared asymmetrically, so the rate means what it says.
//
// scheduleSalt is deliberately not part of the key: a fault has to be the same
// one for a window that already passed as for the window running now, and a salt
// taken from the run would make a reconstruction differ from the live series.
func faultBegan(seed int64, channelId string, index int, slot int64, probability float64) bool {
	return (spreadDraw(seed, faultDrawKey(channelId, index), slot)+1)/2 < probability
}

// faultAt says whether one fault covers at, and at which instant the occurrence
// covering it began - the key a freeze holds its value under and the moment a
// meter exchange captures its offset at.
//
// A drawn occurrence is found without any state: a running one began in one of the
// last lookbackSlots steps, so those are redrawn. The search stops at the first
// step that did begin one, since an older occurrence starts earlier and therefore
// ends earlier - it cannot still be running when this one is not.
func faultAt(fault resolvedFault, seed int64, channelId string, at time.Time) (int64, bool) {
	seconds := at.Unix()
	if fault.windowed {
		if fault.kind == domain.FaultMeterExchange {
			return fault.fromUnix, seconds >= fault.fromUnix
		}
		return fault.fromUnix, seconds >= fault.fromUnix && seconds < fault.toUnix
	}
	slot := seconds / fault.stepSeconds
	for back := int64(0); back < fault.lookbackSlots; back++ {
		if !faultBegan(seed, channelId, fault.index, slot-back, fault.probability) {
			continue
		}
		began := (slot - back) * fault.stepSeconds
		return began, seconds < began+fault.durationSeconds
	}
	return 0, false
}

// faultedReading applies a channel's faults to one raw reading, in document
// order, and reports whether the reading is sent at all.
//
// exchanges holds the captured meter offsets and must not be nil when the channel
// carries a meter exchange. Entries are only ever added, never changed, so a
// caller detects a write by comparing the length before and after.
//
// An outage suppresses the send and the loop still goes on: a meter exchanged or a
// value frozen while the channel is silent is exchanged or frozen when it comes
// back, which is what the hardware does and what keeps the composition independent
// of where the outage sits in the list.
func faultedReading(faults channelFaults, run *faultRun, exchanges map[string]float64, value float64, at time.Time) (float64, bool) {
	send := true
	for i := range faults.list {
		fault := faults.list[i]
		began, active := faultAt(fault, faults.seed, faults.channelId, at)
		if !active {
			continue
		}
		switch fault.kind {
		case domain.FaultOutage:
			send = false
		case domain.FaultFrozen:
			value = run.hold(fault.index, began, value)
		case domain.FaultSpike:
			value *= fault.factor
		case domain.FaultMeterExchange:
			key := meterExchangeKey(faults.channelId, began)
			offset, known := exchanges[key]
			if !known {
				//captured at the first reading at or after the exchange, so the new
				//register starts at reset_to there and counts on from it
				offset = fault.resetTo - value
				exchanges[key] = offset
			}
			value += offset
		}
	}
	return value, send
}

// faultSilences is what is left of the rules for a value that is not a number: a
// string or a boolean has no magnitude to freeze, scale or offset, so an outage is
// the only fault that can act on it. The memory of the other faults is left
// untouched rather than fed a value they cannot hold.
func faultSilences(faults channelFaults, at time.Time) bool {
	for i := range faults.list {
		if faults.list[i].kind != domain.FaultOutage {
			continue
		}
		if _, active := faultAt(faults.list[i], faults.seed, faults.channelId, at); active {
			return true
		}
	}
	return false
}

// faulted is faultedReading at a call site that has an environment: it reads and
// writes the persisted meter offsets, so it must be called with env.mux held. Of
// the five live call sites three already hold it, because every executor calls
// send inside its own run under the mutex, and two take it themselves.
//
// A channel without faults returns its value untouched without looking at
// anything, which is what makes the whole feature additive.
func (this *Runtime) faulted(env *environment, binding channelBinding, run *faultRun, value interface{}, at time.Time) (interface{}, bool) {
	if len(binding.faults.list) == 0 {
		return value, true
	}
	number, numeric := asFloat(value)
	if !numeric {
		return value, !faultSilences(binding.faults, at)
	}
	if binding.faults.exchanges && env.state.MeterExchanges == nil {
		env.state.MeterExchanges = map[string]float64{}
	}
	before := len(env.state.MeterExchanges)
	result, send := faultedReading(binding.faults, run, env.state.MeterExchanges, number, at)
	if len(env.state.MeterExchanges) != before {
		env.dirty = true
	}
	return result, send
}
