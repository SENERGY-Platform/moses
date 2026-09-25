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

package effects

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Unless source maps are disabled, the goja parser reads the file a
// sourceMappingURL comment names. A fifo nobody writes to blocks that read
// forever, so a derivation that returns proves the file was not opened.
func TestASourceMapCommentInAScriptIsNotFollowed(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "map.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("no fifo available: %v", err)
	}
	done := make(chan Graph, 1)
	go func() {
		done <- Derive(scriptSite("moses.environment.state.get('shift');\n//# sourceMappingURL=" + fifo))
	}()
	select {
	case g := <-done:
		expectScriptEdges(t, g, scriptEdge("context:shift", "asset:self", EdgeReads, "shift", 1))
	case <-time.After(5 * time.Second):
		//release the blocked reader before failing, so the goroutine ends
		if writer, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
			writer.Close()
		}
		t.Fatal("the parser opened the file the script's sourceMappingURL names")
	}
}
