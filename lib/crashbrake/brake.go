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

// Package crashbrake turns a fatal crash in a script run from a crash loop into one
// crash: a memory-mapped register records which environment and goroutine is in a
// run, and the next boot quarantines the one the crash report names. Off linux,
// or with an empty dir, it is a no-op.
package crashbrake

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/SENERGY-Platform/moses/lib/util"
)

// SlotCount is how many script runs can be protected in flight at once, far more
// than the few that run concurrently. A run past it is unprotected.
const SlotCount = 1024

// slotSize holds the occupied flag, the goroutine id, the whole environment id
// (bounded by the api) and a truncated channel id: 11 header bytes + 256 for the
// id + 2 + what is left for the channel.
const slotSize = envOffset + envCapacity + 2 + 51

// InFlight is one recovered register entry: the goroutine, environment and
// channel that was inside a script run when the process died.
type InFlight struct {
	Goroutine   uint64
	Environment string
	Channel     string
}

// Decision is one environment the boot check concluded to quarantine, with the
// reason to store and log. Version is the environment's stored version when the
// decision was first kept for a retry, so the next boot can drop it if the
// environment was edited since; zero means a fresh decision, not yet versioned.
type Decision struct {
	Environment string
	Channel     string
	Reason      string
	Version     int64
}

// Brake owns the register and the crash report file of one process. Its methods
// are safe for concurrent use.
type Brake struct {
	dir string
	reg register

	free    []int32 // slot state, 0 free / 1 taken, claimed by atomic CAS
	nextHit atomic.Uint64
	noSlot  sync.Once

	crashLog *os.File
}

// Open maps the register in dir, points crash reports at a file beside it, and
// returns the brake with the environments to quarantine from the previous run.
// An empty dir or a platform without mmap yields a working no-op brake.
func Open(dir string) (*Brake, []Decision, error) {
	brake := &Brake{dir: dir, free: make([]int32, SlotCount)}
	if dir == "" {
		brake.reg = noopRegister{}
		return brake, nil, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("crash brake dir: %w", err)
	}

	reg, err := openRegister(filepath.Join(dir, "register"))
	if err != nil {
		return nil, nil, err
	}
	brake.reg = reg
	inflight, clean := reg.recover()
	//the crash log is truncated at boot below, so any content read here is from the
	//run that just died - no clock comparison is needed to attribute it
	present, report, crashGoroutine := readCrashReport(filepath.Join(dir, "crash.log"))

	//decisions not yet applied from an earlier boot are retried until they are
	pending := readPending(filepath.Join(dir, "pending"))
	decisions := mergeDecisions(pending, decide(inflight, clean, present, report, crashGoroutine))

	//this run starts clean: clear every entry and mark the register not-clean
	reg.reset()

	//truncate on open, after readCrashReport consumed the previous report: one
	//fatal is written per run, so clearing here keeps a stale one from the next boot
	crashLog, err := os.OpenFile(filepath.Join(dir, "crash.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		//do not drop the decisions over a crash-log failure: capture is off for this
		//run, but the previous run's quarantine still has to be applied
		return brake, decisions, fmt.Errorf("crash brake crash log: %w", err)
	}
	brake.crashLog = crashLog
	_ = debug.SetCrashOutput(crashLog, debug.CrashOptions{})
	return brake, decisions, nil
}

// decide quarantines exactly the environments whose in-flight run's goroutine
// matches the crashing goroutine of a Go fatal report. A clean shutdown, no
// report (SIGKILL, OOM, os.Exit), or a report matching no slot quarantines
// nothing.
func decide(inflight []InFlight, cleanShutdown bool, crashReport bool, report string, crashGoroutine uint64) []Decision {
	if cleanShutdown || !crashReport || crashGoroutine == 0 {
		return nil
	}
	var decisions []Decision
	for _, entry := range inflight {
		if entry.Environment == "" || entry.Goroutine != crashGoroutine {
			continue
		}
		decisions = append(decisions, Decision{
			Environment: entry.Environment,
			Channel:     entry.Channel,
			//the channel id may be truncated in the slot, so it is a hint only
			Reason: "a script run in this environment ended the process with a fatal error:\n" + report,
		})
	}
	return decisions
}

// Enter marks a script run in flight in a slot claimed by compare-and-swap and
// returns the release to defer; with every slot taken the run is unprotected.
func (this *Brake) Enter(environment string, channel string) func() {
	if _, ok := this.reg.(noopRegister); ok || this.reg == nil {
		return func() {}
	}
	start := int(this.nextHit.Add(1))
	for probe := 0; probe < SlotCount; probe++ {
		i := (start + probe) % SlotCount
		if atomic.CompareAndSwapInt32(&this.free[i], 0, 1) {
			this.reg.mark(i, goID(), environment, channel)
			return func() {
				this.reg.clear(i)
				atomic.StoreInt32(&this.free[i], 0)
			}
		}
	}
	this.noSlot.Do(func() {
		util.Logger.Warn("the crash brake ran out of slots, some script runs run unprotected", "environment", environment)
	})
	return func() {}
}

// Shutdown records a deliberate stop, so the next boot does not read the register
// as an unclean death.
func (this *Brake) Shutdown() {
	this.reg.markClean()
}

// Close closes the register file and the crash log. The mapping stays, so a
// release still running after Close writes to valid memory.
func (this *Brake) Close() {
	this.reg.close()
	if this.crashLog != nil {
		_ = this.crashLog.Close()
	}
}

// WritePending stores the decisions not yet applied, so the next boot retries
// them; an empty list removes the file.
func (this *Brake) WritePending(decisions []Decision) {
	if this.dir == "" {
		return
	}
	writePending(filepath.Join(this.dir, "pending"), decisions)
}

// goID returns the current goroutine's id from the first line of its stack. It is
// called before the script runs, when the Go stack is shallow, so the walk is
// cheap; the buffer is small so only the header line is formatted.
func goID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	line := buf[:n]
	const prefix = "goroutine "
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return 0
	}
	line = line[len(prefix):]
	if i := bytes.IndexByte(line, ' '); i >= 0 {
		id, _ := strconv.ParseUint(string(line[:i]), 10, 64)
		return id
	}
	return 0
}
