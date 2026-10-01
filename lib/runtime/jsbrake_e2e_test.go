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
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/crashbrake"
)

// A native that converts its receiver to a string, installed as an object's own
// toString, recurses natively without a JavaScript frame and cannot be stopped in
// process. End to end it is one fatal crash, and the next boot quarantines exactly
// the environment whose run it was.
func TestNativeToStringLoopIsQuarantinedByTheBrake(t *testing.T) {
	if dir := os.Getenv("MOSES_TOSTRING_CHILD_DIR"); dir != "" {
		debug.SetMaxStack(32 << 20)
		brake, _, err := crashbrake.Open(dir)
		if err != nil {
			os.Exit(3)
		}
		program, err := compileScript("loop", `var o = {}; o.toString = String.prototype.trim; String(o);`)
		if err != nil {
			os.Exit(4)
		}
		_ = runScriptInBraked(nil, nil, program, map[string]interface{}{}, 5*time.Second, nil, brake, "env-tostring", "ch-1", nil, nil)
		os.Exit(0)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestNativeToStringLoopIsQuarantinedByTheBrake$")
	cmd.Env = append(os.Environ(), "MOSES_TOSTRING_CHILD_DIR="+dir, "GOMEMLIMIT=512MiB")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the child to crash, it exited cleanly:\n%.2000s", out)
	}
	if !strings.Contains(string(out), "stack overflow") && !strings.Contains(string(out), "stack exceeds") {
		t.Fatalf("the child failed, but not with a stack overflow:\n%.2000s", out)
	}
	brake, decisions, err := crashbrake.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer brake.Close()
	if len(decisions) != 1 || decisions[0].Environment != "env-tostring" || decisions[0].Channel != "ch-1" {
		t.Fatalf("expected exactly env-tostring to be quarantined, got %#v", decisions)
	}
}
