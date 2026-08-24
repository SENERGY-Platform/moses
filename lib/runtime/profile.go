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
	"hash/fnv"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

// profileValue is the value a profile channel publishes at t.
//
// The spread is a hash of (seed, channelId, time slot), not a running RNG
// stream: a stream's position would depend on how many ticks happened before,
// so a restart would replay different values. A pure function of the clock is
// what makes "same seed, same clock, same values" hold. The slot is the
// publish interval, so the value is stable within one tick window.
func profileValue(p domain.ProfileSource, seed int64, channelId string, intervalSeconds int64, t time.Time) float64 {
	value := p.Base * factorOf(p.HourFactors, t.Hour()) * factorOf(p.WeekdayFactors, mondayBased(t.Weekday()))
	if p.SpreadPercent > 0 {
		slot := t.Unix()
		if intervalSeconds > 0 {
			slot = t.Unix() / intervalSeconds
		}
		value *= 1 + (p.SpreadPercent/100)*spreadDraw(seed, channelId, slot)
	}
	return value
}

// factorOf treats an absent factor list as neutral; validation guarantees the
// length is exact otherwise.
func factorOf(factors []float64, index int) float64 {
	if len(factors) == 0 {
		return 1
	}
	return factors[index]
}

// mondayBased maps Go's sunday-first weekday to the monday-first convention the
// document format declares.
func mondayBased(day time.Weekday) int {
	return (int(day) + 6) % 7
}

// spreadDraw is a deterministic draw in [-1, 1).
func spreadDraw(seed int64, channelId string, slot int64) float64 {
	h := fnv.New64a()
	var buf [16]byte
	for i := 0; i < 8; i++ {
		buf[i] = byte(uint64(seed) >> (8 * i))
		buf[8+i] = byte(uint64(slot) >> (8 * i))
	}
	h.Write(buf[:])
	h.Write([]byte(channelId))
	// top 53 bits, the float64 mantissa, mapped onto [-1, 1)
	return float64(h.Sum64()>>11)/float64(1<<52) - 1
}
