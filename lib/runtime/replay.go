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
		if virtual < first || virtual > last {
			return 0, false
		}
	default: //loop
		elapsed := now.Unix() - anchorUnix
		if elapsed < 0 {
			return 0, false
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

// slotSeconds is the length of the interval a sample covers: the distance to
// the next point, or for the last point the distance to the previous one,
// because a last slot has no end of its own.
func slotSeconds(points []dataset.Point, index int) int64 {
	if index+1 < len(points) {
		return points[index+1].Unix - points[index].Unix
	}
	return points[index].Unix - points[index-1].Unix
}
