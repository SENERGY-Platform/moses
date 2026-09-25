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
	"errors"

	"github.com/SENERGY-Platform/moses/lib/jsguard"
	"github.com/robertkrimen/otto"
)

// maxScriptCallDepth bounds nested calls in one run. otto evaluates on the Go
// stack, so even plain recursion overflows it (~2200 calls under 8 MB); 1000
// levels stay within a few MB, and legacy scripts do not recurse.
const maxScriptCallDepth = 1000

// hardeningSource is the ES5 counterpart of lib/runtime's; otto's JSON.parse
// needs no guard, since Go's decoder stops nesting at 10000 levels.
const hardeningSource = `(function (guardRegExp) {
	var global = (function () { return this; })();
	var defineProperty = Object.defineProperty, classOf = Object.prototype.toString;
	function hide(target, key, value) {
		defineProperty(target, key, {value: value, writable: true, enumerable: false, configurable: true});
	}
	function unavailable(what) {
		return function () { throw new TypeError(what + ' is not available in MOSES scripts'); };
	}

	hide(global, 'eval', unavailable('eval'));
	var blocked = unavailable('the Function constructor');
	blocked.prototype = Function.prototype;
	hide(Function.prototype, 'constructor', blocked);
	hide(global, 'Function', blocked);

	var NativeRegExp = RegExp;
	function isRegExp(value) { return classOf.call(value) === '[object RegExp]'; }
	function patternOf(value) {
		var pattern = value === undefined ? '' : String(value);
		var problem = guardRegExp(pattern);
		if (problem) throw new RangeError(problem);
		return pattern;
	}
	var GuardedRegExp = function RegExp(pattern, flags) {
		return new NativeRegExp(isRegExp(pattern) ? pattern : patternOf(pattern), flags);
	};
	GuardedRegExp.prototype = NativeRegExp.prototype;
	hide(NativeRegExp.prototype, 'constructor', GuardedRegExp);
	hide(global, 'RegExp', GuardedRegExp);

	var nativeMatch = String.prototype.match, nativeSearch = String.prototype.search;
	hide(String.prototype, 'match', function match(regexp) {
		return nativeMatch.call(this, isRegExp(regexp) ? regexp : patternOf(regexp));
	});
	hide(String.prototype, 'search', function search(regexp) {
		return nativeSearch.call(this, isRegExp(regexp) ? regexp : patternOf(regexp));
	});
})`

// hardenVM prepares a fresh vm for an untrusted script; it must run before the
// script and before anything else is bound.
func hardenVM(vm *otto.Otto) error {
	vm.SetStackDepthLimit(maxScriptCallDepth)
	installer, err := vm.Run(hardeningSource)
	if err != nil {
		return err
	}
	if !installer.IsFunction() {
		return errors.New("the script hardening did not evaluate to a function")
	}
	_, err = installer.Call(otto.NullValue(), func(pattern string) string {
		if err := jsguard.RegexpTooComplex(pattern); err != nil {
			return err.Error()
		}
		return ""
	})
	return err
}
