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

package crashbrake

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// encodeEntry stores an id whole (the api bounds it) and decodes back to exactly
// itself, so an id with a 0x1f byte cannot be read as another environment and a
// long id is not spliced into a neighbour.
func TestEncodeDecodeIsExact(t *testing.T) {
	cases := []InFlight{
		{Goroutine: 7, Environment: "env-a", Channel: "ch-1"},
		{Goroutine: 123456789, Environment: "victim\x1fsuffix", Channel: "ch\x1f2"},
		{Goroutine: 1, Environment: strings.Repeat("a", envCapacity), Channel: strings.Repeat("c", 200)},
		{Goroutine: 0, Environment: "", Channel: ""},
	}
	for _, want := range cases {
		slot := make([]byte, slotSize)
		encodeEntry(slot, want.Goroutine, want.Environment, want.Channel)
		got, ok := decodeEntry(slot)
		if !ok {
			t.Fatalf("entry %q did not decode", want.Environment)
		}
		if got.Goroutine != want.Goroutine || got.Environment != want.Environment {
			t.Fatalf("roundtrip changed the entry: got %#v want %#v", got, want)
		}
		//the channel may be truncated, but the environment must be exact
		if !strings.HasPrefix(want.Channel, got.Channel) {
			t.Fatalf("channel changed beyond truncation: got %q want prefix of %q", got.Channel, want.Channel)
		}
	}
}

// A torn write - a length past the slot - decodes to notOK and is never blamed on
// an environment.
func TestDecodeRejectsTornEntry(t *testing.T) {
	slot := make([]byte, slotSize)
	slot[goroutineOffset] = 1
	// an env length larger than the capacity
	slot[envLenOffset] = 0xff
	slot[envLenOffset+1] = 0xff
	if _, ok := decodeEntry(slot); ok {
		t.Fatal("a length past the slot must not decode")
	}
}

// crashingGoroutine reads the running goroutine's id from a report.
func TestCrashingGoroutine(t *testing.T) {
	report := "fatal error: stack overflow\n\nruntime stack:\nruntime.throw(...)\n\ngoroutine 42 [running]:\nmain.f(...)\n"
	if got := crashingGoroutine([]byte(report)); got != 42 {
		t.Fatalf("expected goroutine 42, got %d", got)
	}
	if got := crashingGoroutine([]byte("no goroutine here")); got != 0 {
		t.Fatalf("expected 0 for a report without a goroutine, got %d", got)
	}
}

// decide quarantines exactly the environment whose in-flight goroutine matches
// the crashing one, and nothing on a clean shutdown, no report, or no match.
func TestDecide(t *testing.T) {
	inflight := []InFlight{
		{Goroutine: 7, Environment: "env-a", Channel: "ch-1"},
		{Goroutine: 8, Environment: "env-b", Channel: "ch-2"},
	}
	if d := decide(inflight, true, true, "r", 7); len(d) != 0 {
		t.Fatalf("a clean shutdown decides nothing, got %v", d)
	}
	if d := decide(inflight, false, false, "", 7); len(d) != 0 {
		t.Fatalf("no crash report decides nothing, got %v", d)
	}
	if d := decide(inflight, false, true, "r", 99); len(d) != 0 {
		t.Fatalf("a report matching no slot decides nothing, got %v", d)
	}
	d := decide(inflight, false, true, "boom", 7)
	if len(d) != 1 || d[0].Environment != "env-a" {
		t.Fatalf("expected only env-a quarantined, got %#v", d)
	}
}

// A released slot is not in flight.
func TestReleaseFreesTheEntry(t *testing.T) {
	dir := t.TempDir()
	brake, _, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	release := brake.Enter("env-a", "ch-1")
	release()
	brake.Close()
	brake2, decisions, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer brake2.Close()
	if len(decisions) != 0 {
		t.Fatalf("a released slot must leave nothing, got %#v", decisions)
	}
}

// A clean shutdown leaves nothing to decide even with a run still marked.
func TestCleanShutdownLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	brake, _, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = brake.Enter("env-a", "ch-1") // not released
	brake.Shutdown()
	brake.Close()
	brake2, decisions, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer brake2.Close()
	if len(decisions) != 0 {
		t.Fatalf("a clean shutdown recovers nothing, got %#v", decisions)
	}
}

// A pending decision that could not be applied is retried on the next boot.
func TestPendingIsRetried(t *testing.T) {
	dir := t.TempDir()
	brake, _, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	brake.WritePending([]Decision{{Environment: "env-a", Reason: "boom"}})
	brake.Close()
	brake2, decisions, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer brake2.Close()
	if len(decisions) != 1 || decisions[0].Environment != "env-a" {
		t.Fatalf("a pending decision must be retried, got %#v", decisions)
	}
}

// Every slot past the count is unprotected: Enter returns a working no-op release
// rather than panicking or corrupting a neighbour.
func TestSlotExhaustion(t *testing.T) {
	dir := t.TempDir()
	brake, _, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer brake.Close()
	releases := make([]func(), 0, SlotCount+1)
	for i := 0; i < SlotCount+1; i++ {
		releases = append(releases, brake.Enter("env", "ch"))
	}
	for _, r := range releases {
		r() // none must panic
	}
}

// End to end: a child marks a run in flight and overflows the Go stack for real;
// the next boot quarantines exactly that environment, matched by goroutine.
func TestARealFatalQuarantinesOnTheNextBoot(t *testing.T) {
	dir := t.TempDir()
	if os.Getenv("MOSES_BRAKE_CHILD") == "1" {
		brake, _, err := Open(os.Getenv("MOSES_BRAKE_DIR"))
		if err != nil {
			os.Exit(3)
		}
		_ = brake.Enter("env-crash", "ch-boom") // not released
		overflow(1)
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestARealFatalQuarantinesOnTheNextBoot$")
	cmd.Env = append(os.Environ(), "MOSES_BRAKE_CHILD=1", "MOSES_BRAKE_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the child to crash, it exited cleanly:\n%s", out)
	}
	brake, decisions, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer brake.Close()
	var q *Decision
	for i := range decisions {
		if decisions[i].Environment == "env-crash" {
			q = &decisions[i]
		}
	}
	if q == nil {
		t.Fatalf("the crashing environment was not quarantined; decisions=%#v", decisions)
	}
	if !strings.Contains(q.Reason, "fatal") && !strings.Contains(q.Reason, "stack") {
		t.Fatalf("expected the crash report in the reason, got %q", q.Reason)
	}
}

//go:noinline
func overflow(n int) int { return n + overflow(n+1) }

// Only a Go fatal error or panic counts as a crash: a SIGQUIT dump, which an
// operator or a probe triggers and which lists a busy goroutine as running, must
// not quarantine the innocent environment that happened to be in flight.
func TestSignalDumpIsNotACrash(t *testing.T) {
	cases := map[string]bool{
		"panic: boom\n\ngoroutine 7 [running]:\n":                                     true,
		"panic: runtime error: invalid memory address\n":                              true,
		"fatal error: concurrent map writes\n":                                        true,
		"\nruntime stack:\nruntime.throw(...)\n\ngoroutine 7 gp=0x1 m=0 [running]:\n": true,
		"runtime: goroutine stack exceeds 268435456-byte limit\n":                     true,
		"SIGQUIT: quit\nPC=0x48b9ad m=0 sigcode=0\n\ngoroutine 7 [running]:\n":        false,
		"": false,
	}
	for report, want := range cases {
		if got := isFatalReport([]byte(report)); got != want {
			t.Errorf("isFatalReport(%q) = %v, want %v", report, got, want)
		}
	}

	dir := t.TempDir()
	brake, _, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = brake.Enter("env-busy", "ch-1")
	brake.Close()
	//the dump names the goroutine that holds the slot, so only the header check
	//keeps it from being quarantined
	dump := "SIGQUIT: quit\nPC=0x1 m=0 sigcode=0\n\ngoroutine " + strconv.FormatUint(goID(), 10) + " [running]:\n"
	if err := os.WriteFile(filepath.Join(dir, "crash.log"), []byte(dump), 0o644); err != nil {
		t.Fatal(err)
	}
	brake2, decisions, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer brake2.Close()
	if len(decisions) != 0 {
		t.Fatalf("a signal dump must not quarantine, got %#v", decisions)
	}
}

// A release that runs after Close - an untracked command dispatch still finishing
// during shutdown - writes into the mapping; Close must leave it mapped, or that
// write is a fatal SIGSEGV that takes this test binary down.
func TestReleaseAfterCloseIsSafe(t *testing.T) {
	brake, _, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	releases := make(chan func(), 64)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			releases <- brake.Enter("env", "ch")
		}()
	}
	wg.Wait()
	close(releases)
	brake.Shutdown()
	brake.Close()
	for release := range releases {
		release()
	}
	again := brake.Enter("env", "ch")
	again()
}

// A fresh decision wins over a pending one for the same environment: the pending
// one may be stale and dropped, which must not take the new crash with it.
func TestAFreshDecisionWinsOverAPendingOne(t *testing.T) {
	pending := []Decision{{Environment: "env-a", Reason: "old", Version: 3}, {Environment: "env-b", Reason: "kept", Version: 1}}
	fresh := []Decision{{Environment: "env-a", Reason: "new"}}
	got := mergeDecisions(pending, fresh)
	if len(got) != 2 || got[0].Environment != "env-a" || got[0].Reason != "new" || got[0].Version != 0 || got[1].Environment != "env-b" {
		t.Fatalf("expected the fresh env-a and the pending env-b, got %#v", got)
	}
}
