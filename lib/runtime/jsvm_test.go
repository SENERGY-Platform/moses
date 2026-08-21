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

func TestAScriptThatRunsTooLongIsStopped(t *testing.T) {
	start := time.Now()
	err := run(`while(true){}`, map[string]interface{}{}, 200*time.Millisecond, nil)
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

	err := run(`var x = 1;`, map[string]interface{}{}, 100*time.Millisecond, mux)
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
	if err := run(`moses.check();`, api, time.Second, mux); err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Error("expected the mutex to be held while the script runs")
	}
}

func TestATimeoutIsNotHandedToTheNextRun(t *testing.T) {
	//the interrupt channel is buffered, so a stale interrupt from a previous run
	//must not be able to reach the next one: every run gets its own vm
	if err := run(`while(true){}`, map[string]interface{}{}, 100*time.Millisecond, nil); !errors.Is(err, ErrScriptTimeout) {
		t.Fatalf("expected the timeout error, got %v", err)
	}
	if err := run(`var x = 1;`, map[string]interface{}{}, time.Second, nil); err != nil {
		t.Errorf("expected the next run to be unaffected, got %v", err)
	}
}

func TestABrokenScriptIsAnErrorAndNotAPanic(t *testing.T) {
	if err := run(`this is not javascript`, map[string]interface{}{}, time.Second, nil); err == nil {
		t.Error("expected a syntax error")
	}
	if err := run(`moses.nothingHere();`, map[string]interface{}{}, time.Second, nil); err == nil {
		t.Error("expected an error for a call into nothing")
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
