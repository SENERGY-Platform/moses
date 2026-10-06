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

// MaxMeterParents bounds one list of meter parents. An equal split over more
// parents would give some of them a weight of 0, which the repository refuses.
const MaxMeterParents = 100

// meterRootId is the id of the root node of the meter graph, graphs.RootNodeId,
// which a meter group id must not take.
const meterRootId = "root"

// EffectiveMeterParents is what the meter graph reads as the parents of an
// asset: its meter_parents, else its submetered_by carrying the whole flow,
// else nothing.
func (this Asset) EffectiveMeterParents() []MeterParent {
	if len(this.MeterParents) > 0 {
		return this.MeterParents
	}
	if this.SubmeteredBy != "" {
		return []MeterParent{{Id: this.SubmeteredBy, Weight: 100}}
	}
	return nil
}

// MeterWeights resolves the weights of one parent list in list order. Explicit
// weights are taken as given; all omitted is an equal split whose remainder
// goes one point each to the first parents. ok is false for a list the
// repository would refuse: mixed, outside 1..100, not summing to exactly 100,
// or longer than MaxMeterParents.
func MeterWeights(parents []MeterParent) (weights []int, ok bool) {
	count := len(parents)
	if count == 0 {
		return nil, true
	}
	if count > MaxMeterParents {
		return nil, false
	}
	omitted := 0
	for _, parent := range parents {
		if parent.Weight == 0 {
			omitted++
		}
	}
	weights = make([]int, count)
	switch omitted {
	case count:
		base, remainder := 100/count, 100%count
		for i := range weights {
			weights[i] = base
			if i < remainder {
				weights[i]++
			}
		}
		return weights, true
	case 0:
		//range first, so the sum below adds at most 100 values of at most 100
		sum := 0
		for i, parent := range parents {
			if parent.Weight < 1 || parent.Weight > 100 {
				return nil, false
			}
			weights[i] = parent.Weight
			sum += parent.Weight
		}
		if sum != 100 {
			return nil, false
		}
		return weights, true
	default:
		return nil, false
	}
}
