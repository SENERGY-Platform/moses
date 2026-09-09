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
	"sort"
	"time"

	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/domain"
)

// replayValue is the value a dataset channel publishes at now. The bool is
// false when there is nothing to play: an original-anchored series outside its
// time range stays silent rather than inventing a value.
//
// anchorUnix is the persisted start of the replay (see RuntimeState.Anchors):
// with the loop anchor the series plays relative to it and repeats, so a
// restart resumes mid-loop instead of starting over.
func replayValue(source domain.DatasetSource, points []dataset.Point, anchorUnix int64, now time.Time, tickSeconds int64) (float64, bool) {
	first, last := points[0].Unix, points[len(points)-1].Unix
	span := last - first

	var virtual int64
	loops := int64(0)
	switch source.Anchor {
	case domain.AnchorOriginal:
		virtual = now.Unix()
		if virtual < first {
			return 0, false
		}
		if virtual > last {
			if !followHolds(source, last, virtual) {
				return 0, false
			}
			//a following source is re-read on its own cadence, so between two
			//refreshes the newest measurement is what the present holds -
			//without this the channel would be silent for all but the instant
			//of its last fetched point
			virtual = last
		}
	default: //loop
		elapsed := now.Unix() - anchorUnix
		if elapsed < 0 {
			return 0, false
		}
		if span <= 0 {
			//a series whose points all sit on one second has nothing to loop
			//over; dividing by its span would panic the tick. It replays as
			//the constant it is.
			virtual = first
			break
		}
		loops = elapsed / span
		virtual = first + elapsed%span
	}

	value := resample(source.Resample, points, virtual, tickSeconds)
	if source.Cumulative && loops > 0 {
		//a meter reading keeps counting across the loop boundary: every
		//completed loop contributes the full sweep of the series
		value += float64(loops) * (points[len(points)-1].Value - points[0].Value)
	}
	if source.Scale != 0 {
		value *= source.Scale
	}
	return value, true
}

func resample(mode domain.ResampleMode, points []dataset.Point, virtual int64, tickSeconds int64) float64 {
	//index of the first point after virtual
	next := sort.Search(len(points), func(i int) bool { return points[i].Unix > virtual })
	previous := next - 1 //>= 0, virtual is never before the first point

	switch mode {
	case domain.ResampleLinear:
		if next >= len(points) {
			return points[previous].Value
		}
		p, n := points[previous], points[next]
		fraction := float64(virtual-p.Unix) / float64(n.Unix-p.Unix)
		return p.Value + fraction*(n.Value-p.Value)
	case domain.ResampleDistribute:
		//the sample at a point is the quantity of [point, next point); a tick
		//gets its share, so summing the ticks of a slot yields the sample
		slot := slotSeconds(points, previous)
		return points[previous].Value * float64(tickSeconds) / float64(slot)
	default: //hold
		return points[previous].Value
	}
}

// followHolds says whether a replay may still answer with the newest point of
// a following source at an instant after it: only for a source that follows,
// only while that point is at most followStaleMultiplier cadences old, and not
// for "distribute", which hands out the share of a slot rather than a value -
// holding it would keep integrating the last sample's quantity into every slot
// after the series ended.
func followHolds(source domain.DatasetSource, lastUnix int64, nowUnix int64) bool {
	if !source.Follow || source.Resample == domain.ResampleDistribute {
		return false
	}
	return nowUnix-lastUnix <= followHoldSeconds(source)
}

// followHoldSeconds is how long past its newest point a following source keeps
// answering: followStaleMultiplier refresh cadences, rounded up to a whole
// second because the instants of a series are whole seconds, and at least one,
// since a bound below a second could not be held at all.
func followHoldSeconds(source domain.DatasetSource) int64 {
	every, err := domain.ParseFollowEvery(source.FollowEvery)
	if err != nil {
		//validation refuses an unreadable follow_every, so this document
		//bypassed the api; the default cadence is what the loop assumes too
		every, _ = domain.ParseFollowEvery("")
	}
	//in whole seconds before the multiplication, so that a window of centuries
	//cannot overflow the duration
	seconds := int64(every / time.Second)
	if every%time.Second != 0 {
		seconds++
	}
	hold := followStaleMultiplier * seconds
	if hold < 1 {
		hold = 1
	}
	return hold
}

// slotSeconds is the length of the interval a sample covers: the distance to
// the next point, or for the last point the distance to the previous one,
// because a last slot has no end of its own.
func slotSeconds(points []dataset.Point, index int) int64 {
	if index+1 < len(points) {
		return points[index+1].Unix - points[index].Unix
	}
	return points[index].Unix - points[index-1].Unix
}
