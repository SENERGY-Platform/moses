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

package jsguard

import (
	"strings"
	"testing"
)

func deepParens(n int) string {
	return strings.Repeat("(", n) + "1" + strings.Repeat(")", n)
}

func mustDepth(t *testing.T, code string) int {
	t.Helper()
	d, err := scriptDepth(code)
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	return d
}

func TestScriptTooComplexDepthBoundary(t *testing.T) {
	if err := ScriptTooComplex(deepParens(MaxScriptDepth)); err != nil {
		t.Fatalf("a script nested exactly to the limit was refused: %v", err)
	}
	if err := ScriptTooComplex(deepParens(MaxScriptDepth + 1)); err == nil {
		t.Fatal("a script one level past the limit was accepted")
	}
}

func TestScriptTooComplexLengthBoundary(t *testing.T) {
	if err := ScriptTooComplex(strings.Repeat(" ", MaxScriptBytes)); err != nil {
		t.Fatalf("a script exactly at the size limit was refused: %v", err)
	}
	if err := ScriptTooComplex(strings.Repeat(" ", MaxScriptBytes+1)); err == nil {
		t.Fatal("a script one byte past the size limit was accepted")
	}
}

// TestRegexpLiteralsDoNotHideNesting pins the verifier's four inputs: a quote, a
// backtick, an escaped slash or a comment opener inside a regexp literal must not
// make the scan skip the code after it.
func TestRegexpLiteralsDoNotHideNesting(t *testing.T) {
	deep := deepParens(MaxScriptDepth + 50)
	cases := map[string]string{
		"escaped slash": "var q=/\\//; var x=" + deep + ";",
		"quote":         "var q=/'/; var x=" + deep + "; var z=/'/;",
		"backtick":      "var q=/`/;\nvar x=" + deep + ";\nvar z=/`/;",
		"comment chars": "var q=/[/*]/;\nvar x=" + deep + ";\nvar z=/[*/]/;",
	}
	for name, code := range cases {
		if d := mustDepth(t, code); d < MaxScriptDepth+50 {
			t.Errorf("%s: depth %d, want at least %d", name, d, MaxScriptDepth+50)
		}
		if ScriptTooComplex(code) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestRegexpLiteralsDoNotInflateNesting pins the two legitimate scripts that were
// refused: a bracket inside a class or escaped in a regexp is not a level that
// stays open.
func TestRegexpLiteralsDoNotInflateNesting(t *testing.T) {
	replaceLines := "var label='a(b';\n" + strings.Repeat("label = label.replace(/[(]/g, \"\");\n", 100)
	if d := mustDepth(t, replaceLines); d > 3 {
		t.Errorf("replace with a class: depth %d, want at most 3", d)
	}
	split := strings.Repeat("function strip(s) { return s.split(/\\(/)[0]; }\n", 300)
	if d := mustDepth(t, split); d > 4 {
		t.Errorf("split with an escaped paren: depth %d, want at most 4", d)
	}
	if err := ScriptTooComplex(replaceLines); err != nil {
		t.Errorf("replace script refused: %v", err)
	}
	if err := ScriptTooComplex(split); err != nil {
		t.Errorf("split script refused: %v", err)
	}
}

func TestRegexpGroupsCount(t *testing.T) {
	code := "var r = /" + strings.Repeat("(", MaxScriptDepth+1) + "a" + strings.Repeat(")", MaxScriptDepth+1) + "/;"
	if ScriptTooComplex(code) == nil {
		t.Fatal("deeply nested regexp groups were accepted")
	}
}

func TestDivisionIsNotARegexp(t *testing.T) {
	// if these slashes were read as regexp delimiters the parens between them
	// would be skipped as class content or popped
	code := "var a = 4, b = 2; var x = a / b[0] / " + deepParens(MaxScriptDepth+1) + ";"
	if ScriptTooComplex(code) == nil {
		t.Fatal("nesting after a division was not counted")
	}
	code = "var x = (a) / (b) / 2; var y = f(1) / g[2] / 3;"
	if d := mustDepth(t, code); d > 1 {
		t.Errorf("divisions: depth %d, want 1", d)
	}
}

func TestControlConditionAllowsARegexp(t *testing.T) {
	// after the condition of if a "/" starts a regexp; reading it as a division
	// would put the quote in code and hide the rest of the line in a string
	code := "if (x) /'/.test(s); var y=" + deepParens(MaxScriptDepth+1) + ";"
	if ScriptTooComplex(code) == nil {
		t.Fatal("nesting after a regexp following if(...) was not counted")
	}
}

// TestAmbiguousRegexpCountsItsBrackets: after "}" a "/" may be a division, in
// which case the class content would be code, so it counts.
func TestAmbiguousRegexpCountsItsBrackets(t *testing.T) {
	code := "var x = {} / b[" + strings.Repeat("(", MaxScriptDepth+1) + "1" + strings.Repeat(")", MaxScriptDepth+1) + "] / 1;"
	if ScriptTooComplex(code) == nil {
		t.Fatal("brackets in an ambiguous regexp span were not counted")
	}
}

func TestStringsAndCommentsDoNotCount(t *testing.T) {
	code := `var s = "((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((";
var t = '[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[';
// ((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((
/* {{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{{ */
var u = "a\"(((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((((";
moses.service.send(s.length + t.length + u.length);`
	if d := mustDepth(t, code); d > 1 {
		t.Errorf("brackets in strings or comments counted: depth %d", d)
	}
}

func TestTemplateSubstitutionCounts(t *testing.T) {
	code := "var t = `${" + deepParens(MaxScriptDepth) + "}`;"
	if ScriptTooComplex(code) == nil {
		t.Fatal("nesting inside a template substitution was not counted")
	}
	code = "var t = `a ${ `b ${ c } d` } e (((((((( `; var u = 1;"
	if d := mustDepth(t, code); d != 2 {
		t.Errorf("nested template: depth %d, want 2", d)
	}
}

// TestStrayCloserResynchronises: a closer that does not match the top pops back
// to its opener, so stray openers do not pile up across a script.
func TestStrayCloserResynchronises(t *testing.T) {
	code := strings.Repeat("{ ( }\n", MaxScriptDepth+10)
	if d := mustDepth(t, code); d > 2 {
		t.Errorf("stray openers piled up: depth %d", d)
	}
}

func TestLineTerminatorsEndLineComments(t *testing.T) {
	for _, term := range []string{"\n", "\r", "\u2028", "\u2029"} {
		code := "// comment" + term + "var x=" + deepParens(MaxScriptDepth+1) + ";"
		if ScriptTooComplex(code) == nil {
			t.Errorf("line comment ended by %q hid the next line", term)
		}
	}
}

func TestU2028StaysInsideAString(t *testing.T) {
	// goja allows U+2028 raw in a string, so it must not end the string and let
	// the backtick after it open a template that hides the rest
	code := "var s = 'a\u2028`';\nvar x=" + deepParens(MaxScriptDepth+1) + ";"
	if ScriptTooComplex(code) == nil {
		t.Fatal("U+2028 inside a string desynchronised the scan")
	}
}

func TestHashbangIsACommentAndHTMLCommentsAreNot(t *testing.T) {
	if d := mustDepth(t, "#! ((((((((\nvar x = 1;"); d != 0 {
		t.Errorf("hashbang counted: depth %d", d)
	}
	// goja has no HTML-like comments, so "<!--" is code and the parens after it count
	code := "var a = 1 <!-- ((((((\n"
	if d := mustDepth(t, code); d < 6 {
		t.Errorf("<!-- treated as a comment: depth %d", d)
	}
}

func TestPropertyNamedLikeAKeywordIsAnIdentifier(t *testing.T) {
	// x.return is a property, so the "/" after it is a division and the parens count
	code := "var q = x.return / b[" + deepParens(MaxScriptDepth+1) + "] / 1;"
	if ScriptTooComplex(code) == nil {
		t.Fatal("a property named return was read as the keyword")
	}
}

func TestTooManyUnterminatedRegexpsAreRefused(t *testing.T) {
	code := strings.Repeat("x = /[ ;\n", maxRegexpFallbacks+1)
	if _, err := scriptDepth(code); err == nil {
		t.Fatal("unbounded regexp fallbacks were accepted")
	}
	code = strings.Repeat("x = /[ ;\n", maxRegexpFallbacks)
	if _, err := scriptDepth(code); err != nil {
		t.Fatalf("fallbacks within the bound were refused: %v", err)
	}
}

func TestRealScriptShapesPass(t *testing.T) {
	scripts := []string{
		`moses.service.send(1);`,
		`var v = moses.device.state.get("temp"); moses.service.send(Math.max(0, v * (1 + 0.1)));`,
		"var t = `value ${ Math.round( (1+2) * 3 ) } done`; moses.service.send(t.length);",
		`if (a) { if (b) { for (var i=0;i<n;i++) { f(g(h(i))); } } }`,
		`var r = /^[a-z]+\/(\d+)$/i.exec(s); var q = a / 2 / b; var o = {a: [1, {b: 2}]};`,
		`for (const x of /a/g[Symbol.split]("bab")) {} var z = x => x / 2;`,
		`function f() { return /[)]/.test(s) } var k = f() / 2;`,
	}
	for _, s := range scripts {
		if err := ScriptTooComplex(s); err != nil {
			t.Errorf("real script refused: %v\nscript: %s", err, s)
		}
	}
}
