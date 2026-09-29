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
	"strings"
	"testing"

	"github.com/robertkrimen/otto"
)

// The reimplemented JSON.stringify must be output-identical to otto's native one
// on ordinary values.
func TestOttoStringifyParity(t *testing.T) {
	exprs := []string{
		`JSON.stringify(42)`,
		`JSON.stringify("a\"b\n")`,
		`JSON.stringify([1,2,3])`,
		`JSON.stringify({b:1,a:2,c:[1,2]})`,
		`JSON.stringify([1,null,3])`,
		`JSON.stringify({a:{b:{c:1}}})`,
		`JSON.stringify({a:1,b:2,c:3}, ['a','c'])`,
		//otto orders the members by key even when a replacer array lists them
		`JSON.stringify({b:1,a:2,c:3}, ['c','a'])`,
		`JSON.stringify({b:1,a:2}, ['b','a','b'])`,
		`JSON.stringify({x:{b:1,a:2}}, ['x','b','a'])`,
		`JSON.stringify({a:1,b:2}, function(k,v){ return k==='a'?undefined:v })`,
		`JSON.stringify({a:1,b:[2,3]}, null, 2)`,
		`JSON.stringify({toJSON:function(){return {x:1}}})`,
	}
	native := otto.New()
	hardened := otto.New()
	if err := hardenVM(hardened); err != nil {
		t.Fatal(err)
	}
	for _, expr := range exprs {
		want, err := native.Run(expr)
		if err != nil {
			t.Fatalf("native %q errored: %v", expr, err)
		}
		got, err := hardened.Run(expr)
		if err != nil {
			t.Fatalf("hardened %q errored: %v", expr, err)
		}
		if want.String() != got.String() {
			t.Errorf("%q:\n native %q\n ours   %q", expr, want.String(), got.String())
		}
	}
}

// convertOttoValue reads each property once and bounds depth, node count and total
// string bytes, so a deep value and a getter returning a gigabyte string are both
// refused rather than exhausting the stack or memory.
func TestConvertOttoValueBoundsDeepAndHugeValues(t *testing.T) {
	vm := otto.New()
	deep, err := vm.Run("var a = {}; for (var i = 0; i < 5000; i++) a = {n: a}; a;")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := convertOttoValue(deep, 33, 10000); err == nil {
		t.Fatal("a deep value must be refused")
	}
	huge, err := vm.Run("var o = {}; Object.defineProperty(o, 'x', {enumerable:true, get:function(){ return new Array(20000000).join('x'); }}); o;")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := convertOttoValue(huge, 1000, 1000000); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("a huge string from a getter must be refused for size, got %v", err)
	}
}
