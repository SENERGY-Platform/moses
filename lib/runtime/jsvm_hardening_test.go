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
	"bytes"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/jsguard"
	"github.com/dop251/goja"
	"github.com/dop251/goja/parser"
)

// TestCompileScriptRejectsDeepNestingBeforeParse: this script would parse fine
// under the default stack, so only a guard ahead of the parser can refuse it.
func TestCompileScriptRejectsDeepNestingBeforeParse(t *testing.T) {
	code := strings.Repeat("(", 5000) + "1" + strings.Repeat(")", 5000)
	_, err := compileScript("deep", code)
	if err == nil {
		t.Fatal("compileScript accepted a script past the depth limit")
	}
	if !strings.Contains(err.Error(), "deep") {
		t.Fatalf("expected a complexity error, got %v", err)
	}
}

// TestLengthLimitBoundsTheParserStack pins the per-byte stack cost the size limit
// relies on (brackets ~2.1 KB, uncounted chains far less), so a goja upgrade that raises it fails here.
func TestLengthLimitBoundsTheParserStack(t *testing.T) {
	r := strings.Repeat
	fill := func(unit, tail string) string {
		return r(unit, (jsguard.MaxScriptBytes-len(tail))/len(unit)) + tail
	}
	cases := []struct {
		name    string
		code    string
		stackMB int
	}{
		{"unclosed parens", r("(", 8<<10), 32},
		{"unclosed brackets", r("[", 8<<10), 32},
		{"unclosed calls", r("a(", 4<<10), 32},
		{"unclosed objects", "x=" + r("{a:", (8<<10)/3), 32},
		{"unary chain", fill("!", "1"), 16},
		{"arrow chain", fill("x=>", "1"), 16},
		{"conditional chain", fill("a?b:", "c"), 16},
		{"assignment chain", fill("a=", "1"), 16},
		{"exponent chain", fill("a**", "a"), 16},
		{"if chain", fill("if(1)", ";"), 16},
	}
	if name := os.Getenv("MOSES_PARSER_STACK_CHILD"); name != "" {
		for _, c := range cases {
			if c.name == name {
				debug.SetMaxStack(c.stackMB << 20)
				_, _ = goja.Parse(c.name, c.code, parser.WithDisableSourceMaps)
				os.Exit(0)
			}
		}
		os.Exit(3)
	}
	for _, c := range cases {
		if len(c.code) > jsguard.MaxScriptBytes {
			t.Fatalf("%s: input of %d bytes exceeds the limit", c.name, len(c.code))
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestLengthLimitBoundsTheParserStack$")
		cmd.Env = append(os.Environ(), "MOSES_PARSER_STACK_CHILD="+c.name, "GOMEMLIMIT=512MiB")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s (%d bytes) did not parse under %d MB: %v\n%s", c.name, len(c.code), c.stackMB, err, lastLines(string(out), 3))
		}
	}
}

// TestUnguardedDeepScriptCrashesTheParser shows, in a child under a small max
// stack, that goja's parser dies on the script compileScript refuses.
func TestUnguardedDeepScriptCrashesTheParser(t *testing.T) {
	const depth = 60000
	code := strings.Repeat("(", depth) + "1" + strings.Repeat(")", depth)

	if os.Getenv("MOSES_PARSER_CRASH_CHILD") == "1" {
		// a small max stack makes the overflow happen at a shallow depth and
		// with little memory; the input above overflows it many times over
		debug.SetMaxStack(8 << 20)
		_, _ = goja.Compile("crash", code, false)
		// reached only if the parser did NOT overflow
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestUnguardedDeepScriptCrashesTheParser$", "-test.v")
	cmd.Env = append(os.Environ(), "MOSES_PARSER_CRASH_CHILD=1", "GOMEMLIMIT=512MiB")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the child to crash parsing a deep script, it exited cleanly:\n%s", out)
	}
	if !bytes.Contains(out, []byte("stack overflow")) {
		t.Fatalf("child failed but not with a stack overflow:\n%s", out)
	}

	// the guarded path must refuse the same script rather than crash
	if _, err := compileScript("guarded", code); err == nil {
		t.Fatal("compileScript accepted a crash-deep script")
	}
}
