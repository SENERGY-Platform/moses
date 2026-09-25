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
	"runtime/debug"
	"strings"
	"testing"
)

// The parser overflows the stack on deep nesting, a fatal error that ends the
// process. With the stack capped at 64 MiB the unguarded parser dies on this
// input at once, so the test also stays cheap when it fails.
func TestTheParserIsNeverHandedAnOverflowingScript(t *testing.T) {
	previous := debug.SetMaxStack(64 << 20)
	t.Cleanup(func() { debug.SetMaxStack(previous) })
	for _, c := range []struct {
		code, reason string
	}{
		{strings.Repeat("(", 300000) + "1" + strings.Repeat(")", 300000), "too large to analyse"},
		{strings.Repeat("(", maxScriptBytes/2) + "1" + strings.Repeat(")", maxScriptBytes/2-1), "nests too deep to analyse"},
		{"var x = " + strings.Repeat("{a:", maxScriptNesting+1) + "1" + strings.Repeat("}", maxScriptNesting+1), "nests too deep to analyse"},
		{strings.Repeat("`${", maxScriptNesting+1) + "1" + strings.Repeat("}`", maxScriptNesting+1), "nests too deep to analyse"},
	} {
		g := Derive(scriptSite(c.code))
		expectUnresolved(t, g, c.reason)
		if len([]rune(g.Unresolved[0].Expression)) > maxSnippet {
			t.Errorf("expected the expression cut to the snippet length, got %d runes", len([]rune(g.Unresolved[0].Expression)))
		}
	}
}

func TestAScriptAtTheLimitsIsStillAnalysed(t *testing.T) {
	nested := strings.Repeat("(", maxScriptNesting-1) + "moses.environment.state.get('shift')" + strings.Repeat(")", maxScriptNesting-1) + ";"
	padded := nested + "\n//" + strings.Repeat("x", maxScriptBytes-len(nested)-3)
	if len(padded) != maxScriptBytes {
		t.Fatalf("fixture is %d bytes", len(padded))
	}
	g := Derive(scriptSite(padded))
	expectUnresolved(t, g)
	expectScriptEdges(t, g, scriptEdge("context:shift", "asset:self", EdgeReads, "shift", 1))
}

func TestNestingSkipsStringsCommentsTemplatesAndRegularExpressions(t *testing.T) {
	code := "var a = '((((', b = \"[[[[\", c = `((((${ (1) }((((`, d = /[(((]((\\//g; // ((((\n/* {{{{ */ f(1);"
	depth, ok := nesting(code)
	if !ok || depth != 2 {
		t.Errorf("expected a nesting of 2 (the substitution and its parenthesis), got %d, %t", depth, ok)
	}
}

// After a closing parenthesis a '/' may start a regular expression; read as a
// division instead, its closing brackets would hide the openers that follow.
func TestAnAmbiguousSlashIsReadBothWaysAndTheDeeperCounts(t *testing.T) {
	depth, ok := nesting("(((( if (x) /)))/.test(s); ((((")
	if !ok || depth < 8 {
		t.Errorf("expected at least the 8 levels of the regular expression reading, got %d, %t", depth, ok)
	}
	//the other way round: a division whose right side holds openers
	depth, ok = nesting("x = (a) /((((b)))) / 2;")
	if !ok || depth < 4 {
		t.Errorf("expected at least the 4 levels of the division reading, got %d, %t", depth, ok)
	}
}
