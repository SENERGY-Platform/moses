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
	"reflect"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/jsguard"
	"github.com/robertkrimen/otto"
)

// The bounded converter replaced otto's Export on the legacy sinks and gives what
// Export gave on origin/master for every shallow value: an array as a slice, and
// every other object (plain, boxed primitive, arguments, Date, Error) as a map of
// its own enumerable properties.
func TestOttoConverterMatchesTheOldExport(t *testing.T) {
	vm := otto.New()
	for _, expr := range []string{
		"5", "'x'", "true", "null", "[1,{a:2}]", "({a:[1,'x'],b:{c:null}})",
		"new Number(5)", "new Boolean(true)", "new String('s')",
		"(function(){return arguments})(1,2)", "new Date(0)", "/re/g", "new Error('m')",
	} {
		v, err := vm.Run("(" + expr + ")")
		if err != nil {
			t.Fatal(err)
		}
		old, err := v.Export()
		if err != nil {
			t.Fatal(err)
		}
		converted, err := convertOttoValue(v, jsguard.MaxNativeWalkDepth, jsguard.MaxNativeWalkNodes)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if !reflect.DeepEqual(old, converted) {
			t.Errorf("%s: old %#v, new %#v", expr, old, converted)
		}
	}
	//changed on purpose: otto exported a homogeneous number array as []int64,
	//which the old set then refused as not plain; it now stores as a plain slice
	v, _ := vm.Run("[1,2]")
	converted, err := convertOttoValue(v, jsguard.MaxStateDepth+1, jsguard.MaxStateNodes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jsguard.CopyPlainData(converted); err != nil {
		t.Fatalf("a number array must now be storable, got %v", err)
	}
}
