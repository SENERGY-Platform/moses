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

package jsguard

import (
	"testing"
	"time"
)

func TestPlainDataIsAccepted(t *testing.T) {
	for _, v := range []interface{}{nil, true, "x", int64(3), 2.5, []interface{}{1.0, "a", nil},
		map[string]interface{}{"a": []interface{}{map[string]interface{}{"b": false}}}} {
		if err := CheckPlainData(v); err != nil {
			t.Errorf("%#v refused: %v", v, err)
		}
	}
}

func TestAnythingElseIsRefused(t *testing.T) {
	for _, v := range []interface{}{
		func() {},
		map[string]interface{}{"send": func(interface{}) {}},
		[]interface{}{1.0, func() {}},
		time.Unix(0, 0),
		struct{}{},
	} {
		if CheckPlainData(v) == nil {
			t.Errorf("%T accepted", v)
		}
	}
}

func TestPlainDataDepthIsBounded(t *testing.T) {
	var deep interface{} = 1.0
	for i := 0; i < MaxStateDepth; i++ {
		deep = []interface{}{deep}
	}
	if err := CheckPlainData(deep); err != nil {
		t.Fatalf("a value at the depth limit was refused: %v", err)
	}
	if CheckPlainData([]interface{}{deep}) == nil {
		t.Fatal("a value past the depth limit was accepted")
	}
	// a cycle, as a vm exports a self-referencing object, ends at the limit
	cyclic := map[string]interface{}{}
	cyclic["self"] = cyclic
	if CheckPlainData(cyclic) == nil {
		t.Fatal("a cyclic value was accepted")
	}
}

func TestPlainCopyIsDeepAndDropsTheRest(t *testing.T) {
	src := map[string]interface{}{"a": []interface{}{1.0, "x"}, "b": map[string]interface{}{"c": true}}
	copied := mustPlainCopy(t, src).(map[string]interface{})
	// mutating the copy must not reach the source
	copied["a"].([]interface{})[0] = 99.0
	if src["a"].([]interface{})[0] != 1.0 {
		t.Fatal("PlainCopy shared the nested slice")
	}
	// a non-plain leaf is dropped
	withFunc := map[string]interface{}{"ok": 1.0, "f": func() {}}
	out := mustPlainCopy(t, withFunc).(map[string]interface{})
	if out["f"] != nil || out["ok"] != 1.0 {
		t.Fatalf("a function was not dropped: %#v", out)
	}
}

func TestPlainCopyStopsAtACycle(t *testing.T) {
	cyclic := map[string]interface{}{"n": 1.0}
	cyclic["self"] = cyclic
	// must return without recursing forever, and the cycle edge is dropped
	out := mustPlainCopy(t, cyclic).(map[string]interface{})
	if out["n"] != 1.0 {
		t.Fatal("the copy lost the plain data")
	}
	if out["self"] != nil {
		t.Fatal("the cycle was copied instead of dropped")
	}
}

func mustPlainCopy(t *testing.T, value interface{}) interface{} {
	t.Helper()
	copied, err := PlainCopy(value)
	if err != nil {
		t.Fatalf("PlainCopy: %v", err)
	}
	return copied
}
