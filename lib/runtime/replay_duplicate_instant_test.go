package runtime

import (
	"math"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/domain"
)

// A distribute source occasionally receives two rows stamped to the same
// whole second - a duplicate write upstream, or a wrapper answer that is not
// deduplicated. That used to collapse the slot between them to zero width and
// its share to +-Inf; summing the duplicate into the one slot it opens keeps
// the value finite and keeps the total the series carries.
func TestADistributeSeriesWithADuplicateInstantStaysFiniteAndConservesItsTotal(t *testing.T) {
	//two points at the tail share the instant 1800: the recovered slot is
	//[900, 1800), 900 seconds wide, carrying their summed value of 350
	points := []dataset.Point{
		{Unix: 0, Value: 100}, {Unix: 900, Value: 200},
		{Unix: 1800, Value: 300}, {Unix: 1800, Value: 50},
	}
	source := replaySource(domain.ResampleDistribute, domain.AnchorOriginal)

	value, _, playable := replayReading(source, points, 0, time.Unix(1800, 0), 30)
	if !playable {
		t.Fatal("expected the duplicated instant to still play")
	}
	if math.IsInf(value, 0) || math.IsNaN(value) {
		t.Fatalf("a duplicate instant produced %v, expected a finite share", value)
	}

	//summing every 30s tick of the slot the duplicate opens must return
	//exactly what the two points together carry
	const tickSeconds = 30
	var total float64
	for virtual := int64(1800); virtual < 1800+900; virtual += tickSeconds {
		total += resample(domain.ResampleDistribute, points, virtual, tickSeconds)
	}
	if want := 300.0 + 50.0; math.Abs(total-want) > 1e-9 {
		t.Errorf("the slot's ticks sum to %v, want the summed sample %v", total, want)
	}
}
