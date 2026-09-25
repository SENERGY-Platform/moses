//go:build unix

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
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestCompileScriptDoesNotFollowSourceMappingURL: the target is a FIFO nobody
// writes to, so following it would block; the timeout turns that into a failure.
func TestCompileScriptDoesNotFollowSourceMappingURL(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "map")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create a FIFO on this platform: %v", err)
	}
	code := "var a = 1;\n//# sourceMappingURL=file://" + fifo

	done := make(chan error, 1)
	go func() {
		_, err := compileScript("t", code)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("compile of a valid script returned an error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("compileScript blocked reading the sourceMappingURL target; the parser followed it")
	}
}
