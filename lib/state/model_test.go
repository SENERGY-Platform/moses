/*
 * Copyright 2023 InfAI (CC SES)
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

package state

import (
	"math"
	"reflect"
	"testing"
)

func TestCleanStates(t *testing.T) {
	result := CleanStates(map[string]interface{}{
		"a": 42,
		"b": "foo",
		"c": math.NaN(),
	})
	if !reflect.DeepEqual(result, map[string]interface{}{
		"a": 42,
		"b": "foo",
		"c": 0,
	}) {
		t.Error(result)
	}

	world := WorldMsg{Rooms: map[string]RoomMsg{"room": {
		Devices: map[string]DeviceMsg{
			"device": {
				States: map[string]interface{}{
					"a": 42,
					"b": "foo",
					"c": math.NaN(),
				},
			},
		},
	}}}
	world.CleanStates()

	if !reflect.DeepEqual(world.Rooms["room"].Devices["device"].States, map[string]interface{}{
		"a": 42,
		"b": "foo",
		"c": 0,
	}) {
		t.Error(world.Rooms["room"].Devices["device"].States)
	}
}
