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
	"fmt"
	"strings"
	"unicode"
)

// goja's parser recurses per nesting level and overflows the goroutine stack,
// a fatal error no recover catches, at about 255,000 unclosed parentheses (the
// cheapest input found, 1 byte per level) and 231,000 nested object literals.
// 64 KiB is 22 times the largest Musterwerke script (2,948 bytes) and a quarter
// of the smallest overflowing input; 128 levels are 25 times its deepest
// nesting (5) and three orders of magnitude below any overflow.
const (
	maxScriptBytes   = 64 << 10
	maxScriptNesting = 128
	// maxScanStates bounds the lexings followed at once; beyond it the script
	// is refused, which errs on the side of refusing.
	maxScanStates = 64
)

// prescan answers why a script must not reach the parser, or "" when it may.
func prescan(code string) string {
	if len(code) > maxScriptBytes {
		return fmt.Sprintf("the script is too large to analyse (%d bytes, at most %d)", len(code), maxScriptBytes)
	}
	depth, ok := nesting(code)
	if !ok {
		return "the script is too irregular to scan for its nesting, it is not analysed"
	}
	if depth > maxScriptNesting {
		return fmt.Sprintf("the script nests too deep to analyse (at least %d levels, at most %d)", depth, maxScriptNesting)
	}
	return ""
}

type scanMode uint8

const (
	scanCode scanMode = iota
	// scanSlash is a '/' in code whose meaning the next character decides.
	scanSlash
	scanLineComment
	scanBlockComment
	scanBlockStar
	scanSingle
	scanSingleEscape
	scanDouble
	scanDoubleEscape
	scanTemplate
	scanTemplateEscape
	scanTemplateDollar
	scanRegex
	scanRegexEscape
	scanClass
	scanClassEscape
)

// scanState is one way of lexing the script so far. frames holds one rune per
// open template: 0 while in its text, n+1 inside a substitution with n open
// braces of its own.
type scanState struct {
	mode       scanMode
	regexAfter bool
	frames     string
	depth      int
}

func (this scanState) same(other scanState) bool {
	return this.mode == other.mode && this.regexAfter == other.regexAfter && this.frames == other.frames
}

func (this scanState) top() (rune, bool) {
	if this.frames == "" {
		return 0, false
	}
	runes := []rune(this.frames)
	return runes[len(runes)-1], true
}

func (this scanState) withTop(frame rune) scanState {
	runes := []rune(this.frames)
	runes[len(runes)-1] = frame
	this.frames = string(runes)
	return this
}

func (this scanState) pop() scanState {
	runes := []rune(this.frames)
	this.frames = string(runes[:len(runes)-1])
	return this
}

func isLineEnd(r rune) bool {
	return r == '\n' || r == '\r' || r == '\u2028' || r == '\u2029'
}

// nesting is an upper bound of the bracket nesting of code: every '/' that can
// start either a division or a regular expression is lexed both ways, and the
// deepest of all lexings counts. Strings, templates and comments are skipped
// as the lexer of any valid script skips them.
func nesting(code string) (int, bool) {
	states := []scanState{{mode: scanCode, regexAfter: true}}
	deepest := 0
	next := make([]scanState, 0, maxScanStates+2)
	for _, r := range code {
		next = next[:0]
		for _, state := range states {
		merge:
			for _, stepped := range step(state, r) {
				deepest = max(deepest, stepped.depth)
				for i := range next {
					if next[i].same(stepped) {
						next[i].depth = max(next[i].depth, stepped.depth)
						continue merge
					}
				}
				next = append(next, stepped)
			}
		}
		if len(next) == 0 || len(next) > maxScanStates {
			return deepest, false
		}
		if deepest > maxScriptNesting {
			return deepest, true
		}
		states, next = next, states
	}
	return deepest, true
}

func step(state scanState, r rune) []scanState {
	switch state.mode {
	case scanCode:
		return []scanState{code(state, r)}
	case scanSlash:
		switch r {
		case '/':
			state.mode = scanLineComment
			return []scanState{state}
		case '*':
			state.mode = scanBlockComment
			return []scanState{state}
		}
		regex := state
		regex.mode = scanRegex
		result := step(regex, r)
		if !state.regexAfter {
			division := state
			division.mode = scanCode
			division.regexAfter = true
			result = append(result, code(division, r))
		}
		return result
	case scanLineComment:
		if isLineEnd(r) {
			state.mode = scanCode
		}
	case scanBlockComment:
		if r == '*' {
			state.mode = scanBlockStar
		}
	case scanBlockStar:
		switch r {
		case '/':
			state.mode = scanCode
		case '*':
		default:
			state.mode = scanBlockComment
		}
	case scanSingle, scanDouble:
		quote := '\''
		if state.mode == scanDouble {
			quote = '"'
		}
		switch {
		case r == '\\':
			state.mode++
		case r == quote, r == '\n', r == '\r':
			//an unterminated string ends the line as the parser's recovery does
			state.mode = scanCode
			state.regexAfter = false
		}
	case scanSingleEscape, scanDoubleEscape:
		state.mode--
	case scanTemplate:
		switch r {
		case '\\':
			state.mode = scanTemplateEscape
		case '`':
			state = state.pop()
			state.mode = scanCode
			state.regexAfter = false
		case '$':
			state.mode = scanTemplateDollar
		}
	case scanTemplateEscape:
		state.mode = scanTemplate
	case scanTemplateDollar:
		if r == '{' {
			state.frames += string(rune(1))
			state.mode = scanCode
			state.regexAfter = true
			state.depth++
			return []scanState{state}
		}
		state.mode = scanTemplate
		return step(state, r)
	case scanRegex:
		switch {
		case r == '\\':
			state.mode = scanRegexEscape
		case r == '[':
			state.mode = scanClass
		case r == '/', isLineEnd(r):
			state.mode = scanCode
			state.regexAfter = false
		}
	case scanRegexEscape:
		state.mode = scanRegex
	case scanClass:
		switch {
		case r == '\\':
			state.mode = scanClassEscape
		case r == ']':
			state.mode = scanRegex
		case isLineEnd(r):
			state.mode = scanCode
			state.regexAfter = false
		}
	case scanClassEscape:
		state.mode = scanClass
	}
	return []scanState{state}
}

// code steps a state in code mode. regexAfter says whether a '/' that follows
// can only start a regular expression; after a name, a closing bracket or a
// value it can be either and is lexed both ways.
func code(state scanState, r rune) scanState {
	switch {
	case unicode.IsSpace(r) || r == '\u2028' || r == '\u2029':
	case r == '\'':
		state.mode = scanSingle
	case r == '"':
		state.mode = scanDouble
	case r == '`':
		state.frames += string(rune(0))
		state.mode = scanTemplate
	case r == '/':
		state.mode = scanSlash
	case r == '(' || r == '[':
		state.depth++
		state.regexAfter = true
	case r == '{':
		state.depth++
		state.regexAfter = true
		if frame, ok := state.top(); ok && frame > 0 {
			state = state.withTop(frame + 1)
		}
	case r == ')' || r == ']':
		state.depth = max(state.depth-1, 0)
		state.regexAfter = false
	case r == '}':
		state.depth = max(state.depth-1, 0)
		state.regexAfter = false
		if frame, ok := state.top(); ok && frame > 0 {
			if frame == 1 {
				state = state.pop()
				state.mode = scanTemplate
			} else {
				state = state.withTop(frame - 1)
			}
		}
	case r == '+' || r == '-' || r == '_' || r == '$' || r >= 0x80 || strings.ContainsRune("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ", r):
		state.regexAfter = false
	default:
		state.regexAfter = true
	}
	return state
}
