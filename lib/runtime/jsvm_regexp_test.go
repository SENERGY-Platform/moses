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

	"github.com/SENERGY-Platform/moses/lib/jsguard"
)

// TestBacktrackingRegexpEndsWithinTheMatchTimeout: the lookahead puts this
// pattern on regexp2, where the script interrupt does not reach, and the input
// backtracks for far longer than any timeout. The match has to give up by itself.
func TestBacktrackingRegexpEndsWithinTheMatchTimeout(t *testing.T) {
	program, err := compileScript("backtrack", `
		var matched = /^(a+)+(?!x)$/.test("a".repeat(28) + "b");
		moses.result(matched);
	`)
	if err != nil {
		t.Fatal(err)
	}
	var result interface{} = "unset"
	api := map[string]interface{}{"result": func(v interface{}) { result = v }}
	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- runScript(program, api, time.Minute, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * jsguard.RegexpMatchTimeout):
		//left running on purpose: a match that ignores the timeout cannot be stopped
		t.Fatalf("the match was still running after %v", 10*jsguard.RegexpMatchTimeout)
	}
	if elapsed := time.Since(started); elapsed > 4*jsguard.RegexpMatchTimeout {
		t.Fatalf("the match took %v, the timeout is %v", elapsed, jsguard.RegexpMatchTimeout)
	}
	if result != false {
		t.Fatalf("a timed-out match must read as no match, got %v", result)
	}
}
