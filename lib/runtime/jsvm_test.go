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
	"errors"
	"sync"
	"testing"
	"time"
)

// runCode does in one step what the runtime does in two: compileScript when the
// generation is built, runScript on every run.
func runCode(code string, moses interface{}, timeout time.Duration, mux sync.Locker) error {
	program, err := compileScript("test", code)
	if err != nil {
		return err
	}
	return runScript(program, moses, timeout, mux)
}

func TestAScriptThatRunsTooLongIsStopped(t *testing.T) {
	start := time.Now()
	err := runCode(`while(true){}`, map[string]interface{}{}, 200*time.Millisecond, nil)
	if !errors.Is(err, ErrScriptTimeout) {
		t.Fatalf("expected the timeout error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("expected the interrupt to be prompt, took %v", elapsed)
	}
}

// TestWaitingForTheMutexDoesNotCountAgainstTheTimeout is the one behavioural
// difference to lib/state/jsvm.go, and the reason for it: there, the timer was
// armed before the lock was taken, so a script that waited longer than the
// timeout for its turn was killed in its first statement. With one mutex per
// environment and several channels queueing on it, that would turn a busy
// environment into a stream of spurious timeouts.
func TestWaitingForTheMutexDoesNotCountAgainstTheTimeout(t *testing.T) {
	mux := &sync.Mutex{}
	mux.Lock()
	go func() {
		time.Sleep(300 * time.Millisecond)
		mux.Unlock()
	}()

	err := runCode(`var x = 1;`, map[string]interface{}{}, 100*time.Millisecond, mux)
	if err != nil {
		t.Fatalf("expected the script to get its full time limit after the wait, got %v", err)
	}
}

func TestTheScriptRunsUnderTheMutex(t *testing.T) {
	mux := &sync.Mutex{}
	locked := false
	api := map[string]interface{}{
		"check": func() {
			//TryLock is the assertion: if the mutex were free here, two scripts
			//of one environment could interleave
			if mux.TryLock() {
				mux.Unlock()
				return
			}
			locked = true
		},
	}
	if err := runCode(`moses.check();`, api, time.Second, mux); err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Error("expected the mutex to be held while the script runs")
	}
}

func TestATimeoutIsNotHandedToTheNextRun(t *testing.T) {
	//an interrupt that arrives after the run has finished stays on that vm, and
	//a vm interrupted while idle refuses its next program: every run gets its
	//own vm, so a stale interrupt can never reach the following one
	if err := runCode(`while(true){}`, map[string]interface{}{}, 100*time.Millisecond, nil); !errors.Is(err, ErrScriptTimeout) {
		t.Fatalf("expected the timeout error, got %v", err)
	}
	if err := runCode(`var x = 1;`, map[string]interface{}{}, time.Second, nil); err != nil {
		t.Errorf("expected the next run to be unaffected, got %v", err)
	}
}

// TestATimeoutThatFiresAfterTheRunDoesNotReachTheNextOne is the same invariant
// pinned at the one instant it could break: a run that finishes just before its
// own timer fires.
func TestATimeoutThatFiresAfterTheRunDoesNotReachTheNextOne(t *testing.T) {
	for i := 0; i < 200; i++ {
		if err := runCode(`var x = 1;`, map[string]interface{}{}, time.Nanosecond, nil); err != nil && !errors.Is(err, ErrScriptTimeout) {
			t.Fatalf("expected either the script or the timeout, got %v", err)
		}
	}
	if err := runCode(`var x = 1;`, map[string]interface{}{}, time.Second, nil); err != nil {
		t.Errorf("expected a fresh run to be unaffected by the interrupts before it, got %v", err)
	}
}

// TestOneProgramRunsOnManyVmsAtOnce pins the one thing this change newly
// shares: the compiled program sits on the binding, so the same program is run
// by every runner of that channel and, across environments, on more than one
// goroutine at a time. Each run still gets its own vm.
func TestOneProgramRunsOnManyVmsAtOnce(t *testing.T) {
	program, err := compileScript("shared", `
		var total = 0;
		for (var i = 0; i < 200; i++) { total += i; }
		moses.record(total);
	`)
	if err != nil {
		t.Fatal(err)
	}
	var mux sync.Mutex
	var results []interface{}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			api := map[string]interface{}{
				"record": func(value interface{}) {
					mux.Lock()
					defer mux.Unlock()
					results = append(results, value)
				},
			}
			if err := runScript(program, api, 5*time.Second, nil); err != nil {
				t.Errorf("a concurrent run of the shared program failed: %v", err)
			}
		}()
	}
	wg.Wait()
	if len(results) != 32 {
		t.Fatalf("expected 32 results, got %d", len(results))
	}
	for i, value := range results {
		//the sum of 0..199, so a program whose state leaked between vms would
		//show up as a different number rather than as a race alone
		if number, ok := asFloat(value); !ok || number != 19900 {
			t.Errorf("run %d produced %#v (%T) instead of 19900", i, value, value)
		}
	}
}

func TestABrokenScriptIsAnErrorAndNotAPanic(t *testing.T) {
	//the syntax error now comes out of compileScript, at generation build
	if _, err := compileScript("test", `this is not javascript`); err == nil {
		t.Error("expected a syntax error")
	}
	if err := runCode(`this is not javascript`, map[string]interface{}{}, time.Second, nil); err == nil {
		t.Error("expected a syntax error")
	}
	if err := runCode(`moses.nothingHere();`, map[string]interface{}{}, time.Second, nil); err == nil {
		t.Error("expected an error for a call into nothing")
	}
}

// TestAScriptMayUseConsole: otto shipped a console global and goja does not,
// while legacy scripts were migrated verbatim - without the binding the first
// console.log is a ReferenceError that aborts the run before its first send.
func TestAScriptMayUseConsole(t *testing.T) {
	var sent []interface{}
	api := map[string]interface{}{
		"send": func(value interface{}) { sent = append(sent, value) },
	}
	code := `
		console.log("a", 1);
		console.info("b", true);
		console.debug("c", null);
		console.warn("d");
		console.error("e", {x: 1});
		moses.send(1);
	`
	if err := runCode(code, api, time.Second, nil); err != nil {
		t.Fatalf("expected a script that logs to run, got %v", err)
	}
	if len(sent) != 1 {
		t.Fatalf("expected the send after the console calls to happen, got %#v", sent)
	}
	if number, ok := asFloat(sent[0]); !ok || number != 1 {
		t.Errorf("expected the sent value to be 1, got %#v (%T)", sent[0], sent[0])
	}

	//every name a migrated script may call has to be there, not just the one
	//the first script happened to use
	sent = nil
	names := `["log", "info", "debug", "warn", "error"]`
	if err := runCode(`
		var names = `+names+`;
		for (var i = 0; i < names.length; i++) {
			moses.send(typeof console[names[i]]);
		}
	`, api, time.Second, nil); err != nil {
		t.Fatalf("expected the probe script to run, got %v", err)
	}
	if len(sent) != 5 {
		t.Fatalf("expected five probes, got %#v", sent)
	}
	for i, value := range sent {
		if value != "function" {
			t.Errorf("probe %d: expected a function, got %#v", i, value)
		}
	}
}

func TestTrimCodeKeepsBothEnds(t *testing.T) {
	if got := trimCode("short", 10); got != "short" {
		t.Errorf("expected a short script to be kept as it is, got %q", got)
	}
	if got := trimCode("abcdefghij", 10); got != "abcdefghij" {
		t.Errorf("expected a script of exactly the limit to be kept, got %q", got)
	}
	if got := trimCode("abcdefghijkl", 10); got != "abcde[...]hijkl" {
		t.Errorf("expected both ends to survive, got %q", got)
	}
}
