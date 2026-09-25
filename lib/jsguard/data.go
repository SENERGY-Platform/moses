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

import "fmt"

// MaxJSONDepth bounds what JSON.parse is handed: goja's decoder overflows near
// 29000 levels under an 8 MB stack, and real data nests a few dozen levels at
// most. Length is not bounded, a flat document is decoded without recursion.
const MaxJSONDepth = 1000

// MaxRegexpBytes and MaxRegexpDepth bound a pattern compiled at run time. The
// regexp2 compiler overflows near 6000 group levels under an 8 MB stack, while
// flat patterns of any measured length do not recurse.
const (
	MaxRegexpBytes = MaxScriptBytes
	MaxRegexpDepth = MaxScriptDepth
)

// JSONTooComplex refuses text nesting arrays and objects deeper than
// MaxJSONDepth. Brackets inside strings do not count; a decoder stops at the
// first invalid byte, so what follows one does not matter.
func JSONTooComplex(text string) error {
	depth, inString := 0, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inString {
			if c == '\\' {
				i++
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '[', '{':
			depth++
			if depth > MaxJSONDepth {
				return fmt.Errorf("JSON nests deeper than the %d level limit", MaxJSONDepth)
			}
		case ']', '}':
			if depth > 0 {
				depth--
			}
		}
	}
	return nil
}

// RegexpTooComplex refuses a pattern longer than MaxRegexpBytes or nesting groups
// and classes deeper than MaxRegexpDepth, counted as in a regexp literal.
func RegexpTooComplex(pattern string) error {
	if len(pattern) > MaxRegexpBytes {
		return fmt.Errorf("regular expression is %d bytes, larger than the %d byte limit", len(pattern), MaxRegexpBytes)
	}
	groups, class, classLevels := 0, false, 0
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch {
		case c == '\\':
			i++ // an escaped byte is never a bracket; UTF-8 continuation bytes are not ASCII
		case class:
			if c == ']' {
				class, classLevels = false, 0
			} else if c == '[' {
				classLevels++
			}
		case c == '[':
			class, classLevels = true, 1
		case c == '(':
			groups++
		case c == ')':
			if groups > 0 {
				groups--
			}
		}
		if groups+classLevels > MaxRegexpDepth {
			return fmt.Errorf("regular expression nests groups deeper than the %d level limit", MaxRegexpDepth)
		}
	}
	return nil
}
