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
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/domain"
)

// ten minute points around a hole of 6600 seconds, the shape a weather series
// takes when the source archive is missing a stretch of a day
var gapPoints = []dataset.Point{
	{Unix: 0, Value: 10},
	{Unix: 600, Value: 20},
	{Unix: 7200, Value: 80},
	{Unix: 7800, Value: 90},
}

func gapSource(resample domain.ResampleMode, maxGap string) domain.DatasetSource {
	return domain.DatasetSource{
		Origin:   domain.OriginFile,
		Ref:      "d1",
		Resample: resample,
		Anchor:   domain.AnchorLoop,
		MaxGap:   maxGap,
	}
}

func TestAWideGapSilencesEveryResampleMode(t *testing.T) {
	//3900 s into the series is the middle of the hole
	const inside = 3900
	for _, mode := range []domain.ResampleMode{
		domain.ResampleLinear, domain.ResampleHold, domain.ResampleDistribute,
	} {
		t.Run(string(mode), func(t *testing.T) {
			if _, _, playable := replayReading(gapSource(mode, ""), gapPoints, 0, time.Unix(inside, 0), 30); !playable {
				t.Fatal("without a max_gap the hole is bridged, as it always was")
			}
			value, gap, playable := replayReading(gapSource(mode, "1h"), gapPoints, 0, time.Unix(inside, 0), 30)
			if playable {
				t.Fatalf("a hole of 6600 s is wider than the bound of 3600 s, got %v", value)
			}
			if gap.Seconds != 6600 {
				t.Errorf("expected the gap to be reported as 6600 s, got %d", gap.Seconds)
			}
			if gap.StartUnix != 600 {
				t.Errorf("expected the gap to open at 600, got %d", gap.StartUnix)
			}
		})
	}
}

func TestAGapIsWiderThanTheBoundAndNotAsWide(t *testing.T) {
	//the hole between the second and the third point is 6600 s wide
	for _, tc := range []struct {
		maxGap   string
		playable bool
	}{
		{"6601s", true},
		{"6600s", true},
		{"6599s", false},
	} {
		t.Run(tc.maxGap, func(t *testing.T) {
			_, _, playable := replayReading(gapSource(domain.ResampleLinear, tc.maxGap), gapPoints, 0, time.Unix(3900, 0), 30)
			if playable != tc.playable {
				t.Errorf("max_gap %s: expected playable %v, got %v", tc.maxGap, tc.playable, playable)
			}
		})
	}
}

func TestAMeasuredInstantIsNotInAGap(t *testing.T) {
	//the points bounding the hole are measurements, whatever the source did
	//between them, so hold and linear answer with them
	for _, mode := range []domain.ResampleMode{domain.ResampleLinear, domain.ResampleHold} {
		for _, second := range []int64{600, 7200} {
			value, _, playable := replayReading(gapSource(mode, "1h"), gapPoints, 0, time.Unix(second, 0), 30)
			if !playable {
				t.Fatalf("%s at %d: the instant of a point is not in a gap", mode, second)
			}
			if second == 600 && value != 20 {
				t.Errorf("%s at 600: expected the measured 20, got %v", mode, value)
			}
		}
	}
}

func TestTheSeamOfALoopIsNotAGap(t *testing.T) {
	//a series of dense points whose first and last are a loop apart: the
	//distance across the seam is not a distance between two neighbours
	points := []dataset.Point{{Unix: 0, Value: 1}, {Unix: 60, Value: 2}, {Unix: 120, Value: 3}}
	source := gapSource(domain.ResampleLinear, "90s")
	//one second before the loop closes, and the first second of the next round
	for _, second := range []int64{119, 120, 121} {
		if _, _, playable := replayReading(source, points, 0, time.Unix(second, 0), 30); !playable {
			t.Errorf("second %d: the seam of a loop is not a gap", second)
		}
	}
}

func TestTheNewestPointOfAFollowingSourceIsNotInAGap(t *testing.T) {
	//the original anchor clamps to the last point while a following source holds
	//it, and that instant is a measurement rather than the opening of a hole
	source := gapSource(domain.ResampleLinear, "1h")
	source.Anchor = domain.AnchorOriginal
	source.Follow = true
	source.FollowEvery = "30m"
	value, _, playable := replayReading(source, gapPoints, 0, time.Unix(7800+60, 0), 30)
	if !playable {
		t.Fatal("a following source holds its newest point rather than calling it a gap")
	}
	if value != 90 {
		t.Errorf("expected the newest measurement 90, got %v", value)
	}
}

func TestADistributingReplayIsBoundedByItsSlot(t *testing.T) {
	//distribute hands a tick its share of the slot a point opens, so an
	//oversized slot has thinned the sample out at the point itself already
	_, gap, playable := replayReading(gapSource(domain.ResampleDistribute, "1h"), gapPoints, 0, time.Unix(600, 0), 30)
	if playable {
		t.Fatal("the slot of the point at 600 is 6600 s wide and may not be handed out")
	}
	if gap.Seconds != 6600 {
		t.Errorf("expected the slot of 6600 s, got %d", gap.Seconds)
	}
}

func TestAGapIsReportedOncePerGap(t *testing.T) {
	gen := &generation{gapReported: map[string]gapReport{}}
	first := replayGap{StartUnix: 600, Seconds: 6600}
	//the same gap over and over is one event, whatever the tick does
	if !reportGap(gen, "ch-1", first, "1h", false, false) {
		t.Fatal("the first sighting of a gap is reported")
	}
	if reportGap(gen, "ch-1", first, "1h", false, false) {
		t.Error("the same gap is reported once, not once per tick")
	}
	//the next loop of the same series opens the same gap again at a new instant
	if !reportGap(gen, "ch-1", replayGap{StartUnix: 8400, Seconds: 6600}, "1h", false, false) {
		t.Error("a gap at a new instant is a new event")
	}
	//a second source keeps its own bookkeeping
	if !reportGap(gen, "ch-2", first, "1h", false, false) {
		t.Error("a second source is tracked on its own")
	}
}

func TestTheBackfillIsBoundedByTheSameGap(t *testing.T) {
	//the backfill reads through backfillValue rather than through the live tick,
	//and the bound has to hold there too: a window rebuilt after the fact may
	//not carry rows the live path refused to publish
	runtime := &Runtime{}
	gen := &generation{gapReported: map[string]gapReport{}, timeline: newTimelineIndex(domain.Environment{})}
	source := gapSource(domain.ResampleLinear, "1h")
	channel := backfillChannel{channel: domain.Channel{
		Id:     "ch-1",
		Source: domain.Source{Kind: domain.SourceDataset, Dataset: &source},
	}}
	counter := 0.0
	if _, playable := runtime.backfillValue(gen, channel, replaySeries{points: gapPoints}, 0, time.Unix(3900, 0), &counter, false, 30); playable {
		t.Error("the backfill bridged a gap the live path refuses")
	}
	if _, playable := runtime.backfillValue(gen, channel, replaySeries{points: gapPoints}, 0, time.Unix(300, 0), &counter, false, 30); !playable {
		t.Error("the backfill went silent outside the gap")
	}
}
