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

// A looping series whose points all sit on one second used to divide by its
// zero span and panic the tick goroutine - one single-point csv upload took
// the whole service down.
func TestAZeroSpanSeriesReplaysAsAConstantInsteadOfPanicking(t *testing.T) {
	points := []dataset.Point{{Unix: 1000, Value: 42}}
	source := domain.DatasetSource{Anchor: "loop"}
	value, ok := replayValue(source, points, 500, time.Unix(2000, 0), 60)
	if !ok {
		t.Fatal("expected a playable value")
	}
	if value != 42 {
		t.Errorf("expected the constant 42, got %v", value)
	}
}
