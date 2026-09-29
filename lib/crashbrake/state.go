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
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"strings"
)

// crashReportLines is how many lines of the report the reason keeps: enough to
// name the error and the top of the stack without pasting the whole dump.
const crashReportLines = 8

// readCrashReport returns the crashing goroutine's id and the first lines of a Go
// fatal error or panic left by the last run; a signal dump such as SIGQUIT is not a crash.
func readCrashReport(path string) (bool, string, uint64) {
	content, err := os.ReadFile(path)
	if err != nil || len(content) == 0 || !isFatalReport(content) {
		return false, "", 0
	}
	return true, firstLines(string(content), crashReportLines), crashingGoroutine(content)
}

// isFatalReport reports whether the report's first non-empty line is one Go writes
// for a fatal error or panic, not a signal dump such as "SIGQUIT: quit".
func isFatalReport(report []byte) bool {
	scanner := bufio.NewScanner(strings.NewReader(string(report)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		return strings.HasPrefix(line, "panic:") || strings.HasPrefix(line, "fatal error:") ||
			strings.HasPrefix(line, "runtime:") || strings.HasPrefix(line, "runtime stack:")
	}
	return false
}

// crashingGoroutine reads the id from the first "goroutine N [running]:" line,
// falling back to the first "goroutine N [" line. Zero means none was found, which
// quarantines nothing.
func crashingGoroutine(report []byte) uint64 {
	scanner := bufio.NewScanner(strings.NewReader(string(report)))
	var firstAny uint64
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "goroutine ") {
			continue
		}
		rest := strings.TrimPrefix(line, "goroutine ")
		space := strings.IndexByte(rest, ' ')
		if space < 0 {
			continue
		}
		id, err := strconv.ParseUint(rest[:space], 10, 64)
		if err != nil {
			continue
		}
		if strings.Contains(rest, "[running]:") {
			return id
		}
		if firstAny == 0 {
			firstAny = id
		}
	}
	return firstAny
}

func firstLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// readPending reads decisions an earlier boot computed but could not apply; a
// missing or unreadable file is none.
func readPending(path string) []Decision {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var decisions []Decision
	_ = json.Unmarshal(content, &decisions)
	return decisions
}

// writePending stores decisions to retry, or removes the file when there are none.
func writePending(path string, decisions []Decision) {
	if len(decisions) == 0 {
		_ = os.Remove(path)
		return
	}
	if content, err := json.Marshal(decisions); err == nil {
		_ = os.WriteFile(path, content, 0o644)
	}
}

// mergeDecisions unions two decision lists by environment, preferring the fresh
// decision: it carries no version, so a stale pending one cannot drop a new crash.
func mergeDecisions(pending []Decision, fresh []Decision) []Decision {
	seen := map[string]bool{}
	var out []Decision
	for _, d := range append(append([]Decision{}, fresh...), pending...) {
		if d.Environment == "" || seen[d.Environment] {
			continue
		}
		seen[d.Environment] = true
		out = append(out, d)
	}
	return out
}
