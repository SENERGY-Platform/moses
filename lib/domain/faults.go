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

package domain

import "time"

// An injected fault is a defect of the measurement: a meter that goes silent,
// freezes, reads an outlier or is exchanged. It sits between the simulated value
// and the platform, so the simulation itself keeps the undisturbed value and the
// quality of a detection is measurable rather than asserted. See
// docs/injected-faults.md.

// MaxChannelFaults bounds the faults of one channel. An imported document is
// untrusted input, and every fault is evaluated on every computation of the
// channel, so this is a bound on runtime cost as much as on memory.
const MaxChannelFaults = 8

// MaxFaultLookbackSlots bounds how far a drawn occurrence is searched back. A
// running occurrence began within ceil(duration/step) evaluation steps, and that
// number of draws is what one evaluation costs; a longer occurrence is a window
// and is declared as one.
const MaxFaultLookbackSlots = 64

type FaultKind string

const (
	// FaultOutage sends nothing at all while it lasts.
	FaultOutage FaultKind = "outage"
	// FaultFrozen repeats the value of the instant the occurrence began.
	FaultFrozen FaultKind = "frozen"
	// FaultSpike multiplies the reading by Factor.
	FaultSpike FaultKind = "spike"
	// FaultMeterExchange restarts the register at ResetTo and counts on from
	// there. It is one instant rather than a window.
	FaultMeterExchange FaultKind = "meter_exchange"
)

// Fault is one defect of one channel's measurement. It is triggered either by a
// window - From inclusive, To exclusive, both whole seconds like DatedChange -
// or by a rate, PerHour occurrences of DurationSeconds each drawn from the
// environment seed; never by both.
type Fault struct {
	Kind FaultKind `json:"kind" bson:"kind"`

	// From and To are the window. To is exclusive, so two windows meeting at one
	// instant do not both cover it. A meter exchange carries From alone: the new
	// register keeps counting, so there is nothing for To to end.
	From time.Time `json:"from,omitempty" bson:"from,omitempty"`
	To   time.Time `json:"to,omitempty" bson:"to,omitempty"`

	// PerHour and DurationSeconds are the rate. The draw is on the channel's
	// evaluation step and derives from the environment seed, so the same document
	// and clock produce the same occurrences on every path.
	PerHour         float64 `json:"per_hour,omitempty" bson:"per_hour,omitempty"`
	DurationSeconds int64   `json:"duration_seconds,omitempty" bson:"duration_seconds,omitempty,truncate"`

	// Factor is what a spike multiplies the reading by. Zero is a real, named
	// defect - the reading a broken sensor sends - so it is a usable value here
	// and only 1 is refused, since it would be invisible in the series.
	Factor float64 `json:"factor,omitempty" bson:"factor,omitempty"`

	// ResetTo is the reading the new register starts at after an exchange.
	ResetTo float64 `json:"reset_to,omitempty" bson:"reset_to,omitempty"`
}

func validFaultKind(kind FaultKind) bool {
	for _, known := range faultKinds() {
		if kind == known {
			return true
		}
	}
	return false
}

func faultKinds() []FaultKind {
	return []FaultKind{FaultOutage, FaultFrozen, FaultSpike, FaultMeterExchange}
}

// channelStepSeconds is the span one computation of a channel stands for: the
// evaluation cadence of a change trigger, the publish interval without one. It
// is the same number the runtime resolves as channelBinding.stepSeconds, and it
// is what a drawn fault's slot and lookback are counted in.
//
// The two can only differ for a trigger the runtime refuses to use, and such a
// document is refused by checkPublishOnChange in the same pass.
func channelStepSeconds(channel Channel) int64 {
	if channel.PublishOnChange == nil {
		return channel.IntervalSeconds
	}
	if channel.PublishOnChange.EvaluateIntervalSeconds > 0 {
		return channel.PublishOnChange.EvaluateIntervalSeconds
	}
	if channel.Source.IntervalSeconds > 0 {
		return channel.Source.IntervalSeconds
	}
	return channel.IntervalSeconds
}

// CumulativeSource reports whether a source counts up - the only kind of reading
// a meter exchange can restart. Exported because validation and the runtime both
// ask it, and the two answering differently would store a fault the runtime then
// refuses to inject.
func CumulativeSource(source Source) bool {
	switch source.Kind {
	case SourceProfile:
		return source.Profile != nil && source.Profile.Cumulative
	case SourceDataset:
		return source.Dataset != nil && source.Dataset.Cumulative
	}
	return false
}
