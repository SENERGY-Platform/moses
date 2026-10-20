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

	"github.com/dop251/goja"
)

// The replacement join and JSON.stringify must stay linear: they collect parts and
// join them once natively. Each case is timed inside the script, around the one
// call, with a generous bound far above the linear cost and far below the
// quadratic one. The best of three runs counts, so a loaded machine does not
// fail a linear call.
const bestOfThree = `function best(f) { var b = Infinity; for (var r = 0; r < 3; r++) { var t = Date.now(); f(); var d = Date.now() - t; if (d < b) b = d; } return b; }
`

func TestReplacedBuiltinsStayLinear(t *testing.T) {
	cases := map[string]string{
		"join of 1e6 short strings": `var a = []; for (var i = 0; i < 1000000; i++) a.push('ab');
			best(function () { a.join(); })`,
		"String of 1e5 numbers": `var a = []; for (var i = 0; i < 100000; i++) a.push(i);
			best(function () { String(a); })`,
		"stringify of about 1 MB": `var o = []; for (var i = 0; i < 40000; i++) o.push({k: 'value' + i, n: i});
			best(function () { var s = JSON.stringify(o); if (s.length < 1000000) throw new Error('too small ' + s.length); })`,
	}
	for name, code := range cases {
		t.Run(name, func(t *testing.T) {
			vm := goja.New()
			if err := hardenVM(vm); err != nil {
				t.Fatal(err)
			}
			v, err := vm.RunString(bestOfThree + code)
			if err != nil {
				t.Fatal(err)
			}
			ms := v.ToInteger()
			t.Logf("%d ms", ms)
			if ms > 2000 {
				t.Fatalf("%s took %d ms, the linear cost is well under 2000 ms", name, ms)
			}
		})
	}
}
