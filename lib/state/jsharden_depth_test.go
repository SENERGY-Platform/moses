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

package state

import (
	"strconv"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/jsguard"
)

// otto's call limit already stops the Array join family on a deep value, but
// JSON.stringify recurses on the Go stack, and a deep value handed to state.set
// overflows otto's exporter before jsguard sees it. Both have to be refused as a
// catchable error, not a crash.
func TestOttoRefusesADeepValue(t *testing.T) {
	depth := strconv.Itoa(jsguard.MaxNativeWalkDepth + 50)
	repo := &StateRepo{Persistence: notFoundPersistence{}}
	world := &World{Id: "w", States: map[string]interface{}{}}
	api := repo.getJsWorldApi(world)

	stringify := `
		var a = []; for (var i = 0; i < ` + depth + `; i++) a = [a];
		var threw = false;
		try { JSON.stringify(a); } catch (e) { threw = (e instanceof RangeError); }
		moses.world.state.set("stringifyThrew", threw ? 1 : 0);
	`
	if err := run(stringify, api, 5*time.Second, nil, nil, "", "", nil); err != nil {
		t.Fatalf("stringify run failed: %v", err)
	}
	if !isOne(world.States["stringifyThrew"]) {
		t.Fatalf("JSON.stringify did not refuse the deep value: %#v", world.States["stringifyThrew"])
	}

	set := `
		var a = {}; for (var i = 0; i < ` + depth + `; i++) a = {n: a};
		moses.world.state.set("deep", a);
		moses.world.state.set("shallow", 5);
	`
	if err := run(set, api, 5*time.Second, nil, nil, "", "", nil); err != nil {
		t.Fatalf("set run failed: %v", err)
	}
	if _, stored := world.States["deep"]; stored {
		t.Fatal("state.set stored a deep value instead of dropping it")
	}
	if v, ok := world.States["shallow"]; !ok || !isNum(v, 5) {
		t.Fatalf("a shallow value must still be stored, got %#v", world.States["shallow"])
	}
}

func isOne(v interface{}) bool { return isNum(v, 1) }

func isNum(v interface{}, want float64) bool {
	switch n := v.(type) {
	case float64:
		return n == want
	case int64:
		return float64(n) == want
	case int:
		return float64(n) == want
	}
	return false
}
