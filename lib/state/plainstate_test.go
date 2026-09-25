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

import "testing"

// TestALegacySetterDropsWhatIsNotPlainData pins the gate every legacy setter
// passes through: a Go function bound into one routine's vm is not stored.
func TestALegacySetterDropsWhatIsNotPlainData(t *testing.T) {
	if _, ok := plainStateValue("n", 1.5); !ok {
		t.Fatal("plain data was dropped")
	}
	if _, ok := plainStateValue("m", map[string]interface{}{"a": "b"}); !ok {
		t.Fatal("plain data was dropped")
	}
	if _, ok := plainStateValue("f", map[string]interface{}{"send": func(interface{}) {}}); ok {
		t.Fatal("a value carrying a Go function was accepted")
	}
}

// TestALegacyGetterReturnsACopy pins that the legacy world getter hands out a
// copy, so a script cannot write a function into the shared state map through
// the reference get returns.
func TestALegacyGetterReturnsACopy(t *testing.T) {
	world := &World{Id: "w", States: map[string]interface{}{"o": map[string]interface{}{"a": 1.0}}}
	repo := &StateRepo{}
	api := repo.getJsWorldSubApi(world)["state"].(map[string]interface{})
	get := api["get"].(func(field string) interface{})
	got := get("o").(map[string]interface{})
	got["a"] = 999.0
	got["f"] = func() {}
	if world.States["o"].(map[string]interface{})["a"] != 1.0 {
		t.Fatal("the legacy getter handed out the live map")
	}
	if _, hasFunc := world.States["o"].(map[string]interface{})["f"]; hasFunc {
		t.Fatal("a function reached the shared state through the getter")
	}
}

// TestALegacySetterStoresACopy: what a setter stores is not the structure the
// script handed in, so changing that structure later changes nothing stored.
func TestALegacySetterStoresACopy(t *testing.T) {
	given := map[string]interface{}{"a": 1.0}
	stored, ok := plainStateValue("o", given)
	if !ok {
		t.Fatal("plain data was dropped")
	}
	given["f"] = func() {}
	given["a"] = 2.0
	copied := stored.(map[string]interface{})
	if len(copied) != 1 || copied["a"] != 1.0 {
		t.Fatalf("the stored value follows the script's structure: %#v", copied)
	}
}
