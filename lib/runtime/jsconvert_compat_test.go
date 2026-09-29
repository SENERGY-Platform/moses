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
	"reflect"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/jsguard"
	"github.com/dop251/goja"
)

// The bounded converter replaced goja's Export on the api sinks. For every value
// the old path could take without recursing, send and state.set give what they
// gave on origin/master (send: jsNumber of Export; set: jsNumber of CopyPlainData
// of Export). The composites below it changed on purpose and are pinned as such.
func TestConverterMatchesTheOldExportPath(t *testing.T) {
	vm := goja.New()
	oldSend := func(v goja.Value) interface{} { return jsNumber(v.Export()) }
	oldSet := func(v goja.Value) (interface{}, bool) {
		c, err := jsguard.CopyPlainData(v.Export())
		return jsNumber(c), err == nil
	}
	newSend := func(v goja.Value) interface{} {
		c, err := convertScriptValue(v, jsguard.MaxNativeWalkDepth, jsguard.MaxNativeWalkNodes)
		if err != nil {
			return "dropped"
		}
		return jsNumber(c)
	}
	newSet := func(v goja.Value) (interface{}, bool) {
		c, err := convertScriptValue(v, jsguard.MaxStateDepth+1, jsguard.MaxStateNodes)
		if err != nil {
			return nil, false
		}
		p, err := jsguard.CopyPlainData(c)
		return jsNumber(p), err == nil
	}
	same := []string{
		"5", "'x'", "true", "[1,'a',{b:2}]", "({a:[1,2],b:{c:null}})",
		"new Number(5)", "new Boolean(true)", "new Date(0)", "Object(1n)",
		"/re/g", "new Error('m')", "(function(){return arguments})(1,2)",
	}
	for _, expr := range same {
		v, err := vm.RunString("(" + expr + ")")
		if err != nil {
			t.Fatal(err)
		}
		if o, n := oldSend(v), newSend(v); !reflect.DeepEqual(o, n) {
			t.Errorf("send(%s): old %#v, new %#v", expr, o, n)
		}
		ov, ook := oldSet(v)
		nv, nok := newSet(v)
		if ook != nok || (ook && !reflect.DeepEqual(ov, nv)) {
			t.Errorf("set(%s): old %#v/%v, new %#v/%v", expr, ov, ook, nv, nok)
		}
	}
	//changed on purpose: the old Export of these recursed over their content
	//unbounded or exported a Go type with no size limit; a String object exported
	//to an empty map
	changed := map[string]interface{}{
		"new String('s')":      map[string]interface{}{"0": "s"},
		"new Set([1,2])":       "dropped",
		"new Map([[1,2]])":     "dropped",
		"new Int8Array([1,2])": "dropped",
	}
	for expr, want := range changed {
		v, err := vm.RunString("(" + expr + ")")
		if err != nil {
			t.Fatal(err)
		}
		if got := newSend(v); !reflect.DeepEqual(got, want) {
			t.Errorf("send(%s): got %#v, pinned %#v", expr, got, want)
		}
	}
}
