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

// gapReport is what a reporter remembers about the gap a source last reported:
// the instant it opens at and whether a fallback series covered it. Both,
// because a substitute that falls silent inside one gap is a second event.
type gapReport struct {
	startUnix int64
	covered   bool
}

// replaySeries is what one dataset source replays: its own points, and the
// substitute series of a source that declared a fallback, read only inside a
// gap the main one refuses to bridge.
type replaySeries struct {
	points   []dataset.Point
	fallback []dataset.Point
}

// replayReading is the value one series of a dataset source produces at now,
// with the reason behind a silence. playable is false when there is nothing to
// play - an original-anchored series outside its time range stays silent rather
// than inventing a value - and gap is set only for a silence a max_gap caused,
// staying zero for every other silence and for a played value.
//
// anchorUnix is the persisted start of the replay (see RuntimeState.Anchors):
// with the loop anchor the series plays relative to it and repeats, so a
// restart resumes mid-loop instead of starting over.
//
// The live tick and the backfill go through replayWithFallback, which is this
// over a source's two series.
func replayReading(source domain.DatasetSource, points []dataset.Point, anchorUnix int64, now time.Time, tickSeconds int64) (value float64, gap replayGap, playable bool) {
	virtual, loops, inRange := virtualInstant(source, points, anchorUnix, now)
	if !inRange {
		return 0, replayGap{}, false
	}
	if gap, wide := gapAt(source, points, virtual); wide {
		return 0, gap, false
	}
	return adjusted(source, points, resample(source.Resample, points, virtual, tickSeconds), loops), replayGap{}, true
}

// replayWithFallback is replayReading over both series of a source: where the
// main one stands in a gap wider than max_gap, the substitute series is read at
// the same virtual instant, on the same resample mode and the same bound.
//
// gap is set whenever the main series stood in such a gap, whether or not the
// substitute covered it, and covered says the value came from the substitute -
// which is what the one line per gap reports.
//
// Outside the range of the main series - before it starts, after it ends -
// there is no fallback: max_gap is about the middle of a series and not its
// edges, and so is the series that stands in for it.
func replayWithFallback(source domain.DatasetSource, series replaySeries, anchorUnix int64, now time.Time, tickSeconds int64) (value float64, gap replayGap, covered bool, playable bool) {
	value, gap, playable = replayReading(source, series.points, anchorUnix, now, tickSeconds)
	if playable || gap.Seconds == 0 || len(series.fallback) < 2 {
		return value, gap, false, playable
	}
	//the main series stood in a gap, so its virtual instant is inside its range
	//and was computed once already; the substitute is read at exactly that one
	virtual, loops, inRange := virtualInstant(source, series.points, anchorUnix, now)
	if !inRange {
		return 0, gap, false, false
	}
	substitute := series.fallback
	if virtual < substitute[0].Unix || virtual > substitute[len(substitute)-1].Unix {
		//the substitute has no measurement around this instant at all, which is
		//the same silence a hole in it would produce
		return 0, gap, false, false
	}
	if _, wide := gapAt(source, substitute, virtual); wide {
		return 0, gap, false, false
	}
	//the loop offset and the scale are the source's and stay the source's: the
	//substitute stands in for a value of the main series at this instant, so it
	//carries the same adjustments that value would have carried
	return adjusted(source, series.points, resample(source.Resample, substitute, virtual, tickSeconds), loops), gap, true, true
}

// virtualInstant is the instant of the series a replay reads at now: the wall
// clock for an original anchor, the position inside the loop for a loop anchor.
// loops is the number of complete rounds a looping replay has behind it, and
// the bool is false where there is nothing to play - a series that has not
// begun or has ended.
func virtualInstant(source domain.DatasetSource, points []dataset.Point, anchorUnix int64, now time.Time) (virtual int64, loops int64, inRange bool) {
	first, last := points[0].Unix, points[len(points)-1].Unix
	span := last - first

	switch source.Anchor {
	case domain.AnchorOriginal:
		virtual = now.Unix()
		if virtual < first {
			return 0, 0, false
		}
		if virtual > last {
			if !followHolds(source, last, virtual) {
				return 0, 0, false
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
			return 0, 0, false
		}
		if span <= 0 {
			//a series whose points all sit on one second has nothing to loop
			//over; dividing by its span would panic the tick. It replays as
			//the constant it is.
			return first, 0, true
		}
		loops = elapsed / span
		virtual = first + elapsed%span
	}
	return virtual, loops, true
}

// adjusted applies the two source-level corrections a replayed value carries:
// the sweep of the completed loops of a cumulative meter, and the scale.
// points is always the source's own series, since the meter that keeps counting
// is the one the source declares.
func adjusted(source domain.DatasetSource, points []dataset.Point, value float64, loops int64) float64 {
	if source.Cumulative && loops > 0 {
		//a meter reading keeps counting across the loop boundary: every
		//completed loop contributes the full sweep of the series
		value += float64(loops) * (points[len(points)-1].Value - points[0].Value)
	}
	if source.Scale != 0 {
		value *= source.Scale
	}
	return value
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
	//virtual past the last point means there is no distance to bridge: for the
	//source's own series that is the clamp to last, and for a fallback read at
	//the main series' instant it is that series' newest measurement.
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
		//gets its share, so summing the ticks of a slot yields the sample. Points
		//sharing one whole-second instant are the same sample measured twice, so
		//their values are summed into it rather than left to open a slot of zero
		//width - that is what keeps the total the series carries.
		slot := slotSeconds(points, previous)
		if slot == 0 {
			return 0 //every point of the series shares one instant, nothing to distribute over
		}
		return sumRun(points, previous) * float64(tickSeconds) / float64(slot)
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
// because a last slot has no end of its own. Points sharing one whole-second
// instant open no interval among themselves, so the slot is measured from the
// start of that run rather than between two points at the same second.
func slotSeconds(points []dataset.Point, index int) int64 {
	runStart := runStartOf(points, index)
	if index+1 < len(points) {
		return points[index+1].Unix - points[runStart].Unix
	}
	if runStart == 0 {
		return 0 //every point of the series shares one instant, there is no run before it
	}
	return points[runStart].Unix - points[runStart-1].Unix
}

// runStartOf is the first index of the run of points sharing points[index]'s
// instant: a duplicate whole-second timestamp is the same measurement seen
// twice, not a second point opening its own slot.
func runStartOf(points []dataset.Point, index int) int {
	for index > 0 && points[index-1].Unix == points[index].Unix {
		index--
	}
	return index
}

// sumRun adds the values of the points sharing points[index]'s instant - what
// slotSeconds treats as one point's boundary, distribute reads as one sample.
func sumRun(points []dataset.Point, index int) float64 {
	sum := 0.0
	for i := runStartOf(points, index); i <= index; i++ {
		sum += points[i].Value
	}
	return sum
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
//
// A source with a fallback reports the same gap again when the substitute stops
// covering it, or starts to: the bookkeeping is keyed on the opening instant and
// the coverage together, so a substitute that falls silent halfway through a gap
// is a second event rather than a repetition.
func reportGap(gen *generation, id string, gap replayGap, maxGap string, fallback bool, covered bool) bool {
	report := gapReport{startUnix: gap.StartUnix, covered: covered}
	if reported, seen := gen.gapReported[id]; seen && reported == report {
		return false
	}
	gen.gapReported[id] = report
	switch {
	case covered:
		util.Logger.Info("a dataset source reads its fallback series inside a gap wider than its max_gap",
			"source", id, "gap_seconds", gap.Seconds, "max_gap", maxGap)
	case fallback:
		util.Logger.Warn("a dataset source publishes nothing inside a gap wider than its max_gap, its fallback series does not cover it",
			"source", id, "gap_seconds", gap.Seconds, "max_gap", maxGap)
	default:
		util.Logger.Warn("a dataset source publishes nothing inside a gap wider than its max_gap",
			"source", id, "gap_seconds", gap.Seconds, "max_gap", maxGap)
	}
	return true
}
