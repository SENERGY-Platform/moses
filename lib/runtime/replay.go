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
	"github.com/SENERGY-Platform/moses/lib/util"
)

// replayGap is the distance a replay refused to bridge and the instant it opens
// at. The zero value says there was no gap. The opening instant is what a
// reporter keys on: a source stands in one gap for as many ticks as it is wide,
// and a history run walks those in milliseconds.
type replayGap struct {
	StartUnix int64
	Seconds   int64
}

// replayValue is the value a dataset channel publishes at now. The bool is
// false when there is nothing to play: an original-anchored series outside its
// time range stays silent rather than inventing a value.
//
// anchorUnix is the persisted start of the replay (see RuntimeState.Anchors):
// with the loop anchor the series plays relative to it and repeats, so a
// restart resumes mid-loop instead of starting over.
func replayValue(source domain.DatasetSource, points []dataset.Point, anchorUnix int64, now time.Time, tickSeconds int64) (float64, bool) {
	value, _, playable := replayReading(source, points, anchorUnix, now, tickSeconds)
	return value, playable
}

// replayReading is replayValue with the reason behind a silence, for the live
// callers that report it: gap is set only for a silence a max_gap caused, and
// stays zero for every other silence and for a played value.
func replayReading(source domain.DatasetSource, points []dataset.Point, anchorUnix int64, now time.Time, tickSeconds int64) (value float64, gap replayGap, playable bool) {
	first, last := points[0].Unix, points[len(points)-1].Unix
	span := last - first

	var virtual int64
	loops := int64(0)
	switch source.Anchor {
	case domain.AnchorOriginal:
		virtual = now.Unix()
		if virtual < first {
			return 0, replayGap{}, false
		}
		if virtual > last {
			if !followHolds(source, last, virtual) {
				return 0, replayGap{}, false
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
			return 0, replayGap{}, false
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

	if gap, wide := gapAt(source, points, virtual); wide {
		return 0, gap, false
	}

	value = resample(source.Resample, points, virtual, tickSeconds)
	if source.Cumulative && loops > 0 {
		//a meter reading keeps counting across the loop boundary: every
		//completed loop contributes the full sweep of the series
		value += float64(loops) * (points[len(points)-1].Value - points[0].Value)
	}
	if source.Scale != 0 {
		value *= source.Scale
	}
	return value, replayGap{}, true
}

// gapAt is the distance the replay would have to bridge at virtual, and whether
// that distance is wider than the source's max_gap. Without a max_gap nothing
// is a gap, which is what every document written before the field does.
//
// The instant of a point is not in a gap for "hold" and "linear": it is a
// measurement, whatever the source did around it. For "distribute" it is, and
// the distance measured there is the slot rather than the span across virtual -
// that mode hands a tick its share of the slot the point opens, so an oversized
// slot has already thinned the sample out at the slot's first instant.
//
// "hold" is bounded like "linear" on purpose. The mode says a state persists
// between two samples, but max_gap is the document's statement that past this
// distance it no longer vouches for the source at all, and a state a day and a
// half old is as much a claim about an unobserved instant as an interpolated
// one. Keeping the rule the same across the modes also means switching the
// resample mode cannot silently start bridging holes again.
//
// A loop's seam needs no case of its own: virtual runs over [first, last), so
// it never falls between the last point of one round and the first of the next.
func gapAt(source domain.DatasetSource, points []dataset.Point, virtual int64) (replayGap, bool) {
	limit, set, err := domain.ParseMaxGap(source.MaxGap)
	if !set || err != nil {
		//validation refuses an unreadable max_gap, so such a document bypassed
		//the api; a bound nobody can read holds nothing back
		return replayGap{}, false
	}
	//truncated to whole seconds, which is exact rather than lenient: the
	//instants of a series are whole seconds, so an integer distance exceeds a
	//bound of 90.5 s in exactly the cases where it exceeds 90
	seconds := int64(limit / time.Second)
	next := sort.Search(len(points), func(i int) bool { return points[i].Unix > virtual })
	previous := next - 1 //>= 0, virtual is never before the first point
	if source.Resample == domain.ResampleDistribute {
		slot := slotSeconds(points, previous)
		return replayGap{StartUnix: points[previous].Unix, Seconds: slot}, slot > seconds
	}
	//virtual never exceeds the last point - a loop runs over [first, last) and
	//the original anchor either returns early or clamps to last - so the bounds
	//check guards a caller that changes that rather than a case reachable today.
	if next >= len(points) || points[previous].Unix == virtual {
		return replayGap{}, false
	}
	span := points[next].Unix - points[previous].Unix
	return replayGap{StartUnix: points[previous].Unix, Seconds: span}, span > seconds
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

// reportGap warns that a source stands in a gap it will not bridge, once per gap
// rather than once per tick. Must be called with env.mux held, like every other
// touch of a generation's maps.
//
// A silence a max_gap causes is the one silence worth a line: the others are the
// document saying where its series begins and ends, while this one says the
// source has a hole the document refuses to paper over. It was invisible before
// the bound existed, which is why a 27 hour hole in a weather series went
// unnoticed for months.
// The bool says whether this call was the one that reported, which is what a
// test can hold on to: the log line itself is not observable.
func reportGap(gen *generation, id string, gap replayGap, maxGap string) bool {
	if reported, seen := gen.gapReported[id]; seen && reported == gap.StartUnix {
		return false
	}
	gen.gapReported[id] = gap.StartUnix
	util.Logger.Warn("a dataset source publishes nothing inside a gap wider than its max_gap",
		"source", id, "gap_seconds", gap.Seconds, "max_gap", maxGap)
	return true
}
