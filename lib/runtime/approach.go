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
	"math"
	"time"

	"github.com/SENERGY-Platform/moses/lib/repo"
)

// A zone value with a time constant follows a set point instead of jumping to
// it. The law is the single time constant step response of a first order system,
// which is the 1R1C reduction of the RC network a room is usually modelled as:
//
//	value(t) = target + (from - target) * exp(-elapsed / tau)
//
// It is evaluated when the value is read rather than integrated on a ticker.
// That makes it exact for any interval, needs no scheduler of its own, and costs
// nothing for a zone that declares no time constant - which is every zone of
// every document stored so far.
//
// settled is the distance at which the approach is finished and dropped. Without
// it an approach would never end: the exponential reaches its target only in the
// limit, and the entry would be carried and flushed forever.
const settled = 1e-9

// approachOf is the distance covered so far, or the target when the approach is
// finished. The second return says whether it is still running.
func approachOf(a repo.Approach, now time.Time) (float64, bool) {
	if a.TauSeconds <= 0 {
		return a.Target, false
	}
	elapsed := now.Unix() - a.StartUnix
	if elapsed <= 0 {
		//a clock that went backwards must not push the value away from its
		//target; holding the start value is the harmless reading
		return a.From, true
	}
	remaining := (a.From - a.Target) * math.Exp(-float64(elapsed)/float64(a.TauSeconds))
	if math.Abs(remaining) < settled {
		return a.Target, false
	}
	return a.Target + remaining, true
}

// advanceZone writes the current value of every approaching key of one zone into
// the zone state, and drops the ones that have arrived.
//
// The caller holds the environment mutex, as every reader and writer of the
// state maps does.
func (this *environment) advanceZone(zoneId string, now time.Time) {
	running, has := this.state.Approaching[zoneId]
	if !has || len(running) == 0 {
		return
	}
	states := this.zoneStates(zoneId)
	for key, approach := range running {
		value, stillRunning := approachOf(approach, now)
		if states[key] != value {
			states[key] = value
			this.dirty = true
		}
		if !stillRunning {
			delete(running, key)
			this.dirty = true
		}
	}
	if len(running) == 0 {
		delete(this.state.Approaching, zoneId)
	}
}

// startApproach records that a zone value is on its way to target. The value
// itself is left where it is; advanceZone moves it.
//
// A non numeric target cannot be interpolated and is refused by the caller, and
// a current value that is not a number is treated as a start at the target,
// which makes the first set of a fresh key immediate rather than a jump from
// nothing.
func (this *environment) startApproach(zoneId string, key string, target float64, tauSeconds int64, now time.Time) {
	from, ok := asFloat(this.zoneStates(zoneId)[key])
	if !ok {
		from = target
	}
	if this.state.Approaching == nil {
		this.state.Approaching = map[string]map[string]repo.Approach{}
	}
	if this.state.Approaching[zoneId] == nil {
		this.state.Approaching[zoneId] = map[string]repo.Approach{}
	}
	this.state.Approaching[zoneId][key] = repo.Approach{
		From:       from,
		Target:     target,
		StartUnix:  now.Unix(),
		TauSeconds: tauSeconds,
	}
	this.dirty = true
}

// asFloat accepts the number types a state value arrives as: a script writes
// float64, a decoded document may carry an int.
func asFloat(value interface{}) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	}
	return 0, false
}
