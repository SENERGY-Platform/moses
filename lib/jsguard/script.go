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

// Package jsguard bounds what a JavaScript engine is handed to parse or compile:
// their parsers recurse on the Go stack, and a Go stack overflow kills the process.
package jsguard

import (
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

// MaxScriptBytes is the hard bound: goja's parser needs at most ~2.1 KB of stack
// per source byte (unclosed "(" or "["), so 32 KiB stays ~15.9x under Go's 1 GB
// default max stack whatever the depth scan makes of the script.
const MaxScriptBytes = 32 << 10

// MaxScriptDepth bounds bracket nesting, including regexp groups inside regexp
// literals. The deepest real script nests 5 levels; goja overflows near 3600
// levels even under an 8 MB stack.
const MaxScriptDepth = 200

// maxRegexpFallbacks bounds how often an unterminated regexp literal is re-read
// as a division, which keeps the scan linear; a real script has none.
const maxRegexpFallbacks = 64

var errAmbiguous = errors.New("script has too many unterminated regular expression literals to be scanned")

// ScriptTooComplex refuses a script larger than MaxScriptBytes or nesting deeper
// than MaxScriptDepth. It is linear and runs before any parse.
func ScriptTooComplex(code string) error {
	if len(code) > MaxScriptBytes {
		return fmt.Errorf("script is %d bytes, larger than the %d byte limit", len(code), MaxScriptBytes)
	}
	depth, err := scriptDepth(code)
	if err != nil {
		return err
	}
	if depth > MaxScriptDepth {
		return fmt.Errorf("script nests brackets %d deep, deeper than the %d level limit", depth, MaxScriptDepth)
	}
	return nil
}

// scriptDepth returns the deepest nesting of (), [], {}, template substitutions
// and regexp groups. It lexes strings, templates, comments and regexp literals the
// way goja does; whether a "/" starts a regexp is decided from the previous
// significant token, and where that is ambiguous (after "}", of, yield, await)
// the literal is read counting every bracket in it, so either reading of that
// span is covered.
func scriptDepth(code string) (int, error) {
	s := scanner{src: code, regexOK: true}
	if len(code) >= 2 && code[0] == '#' && code[1] == '!' {
		s.i = s.lineEnd(2)
	}
	if err := s.run(); err != nil {
		return 0, err
	}
	return s.deepest, nil
}

// stack markers
const (
	markParen    = '('
	markControl  = 'c' // the condition of if, while, for or with: a "/" after it starts a regexp
	markBracket  = '['
	markBrace    = '{'
	markTemplate = '$'
)

type scanner struct {
	src       string
	i         int
	stack     []byte
	deepest   int
	regexOK   bool // a "/" here starts a regexp literal
	ambiguous bool // regexOK came from a token after which a division is possible too
	afterDot  bool // the next word is a property name, never a keyword
	control   bool // the next "(" opens a control condition
	fallbacks int
}

func (s *scanner) run() error {
	n := len(s.src)
	for s.i < n {
		c := s.src[s.i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			s.i++
		case c >= utf8.RuneSelf:
			r, size := utf8.DecodeRuneInString(s.src[s.i:])
			if r == '\u2028' || r == '\u2029' || unicode.IsSpace(r) || r == '\ufeff' {
				s.i += size
			} else {
				s.word()
			}
		case c == '/' && s.at(1) == '/':
			s.i = s.lineEnd(s.i + 2)
		case c == '/' && s.at(1) == '*':
			s.i = s.blockCommentEnd(s.i + 2)
		case c == '/':
			if s.regexOK {
				if end, ok := s.regexp(s.i+1, s.ambiguous); ok {
					s.i = end
					s.value()
					continue
				}
				// goja errors on an unterminated regexp, so if this parses at all
				// the "/" was a division
				s.fallbacks++
				if s.fallbacks > maxRegexpFallbacks {
					return errAmbiguous
				}
			}
			s.i++
			s.operator()
		case c == '\'' || c == '"':
			s.i = s.stringEnd(s.i+1, c)
			s.value()
		case c == '`':
			s.template(s.i + 1)
		case c == '(':
			if s.control {
				s.push(markControl)
			} else {
				s.push(markParen)
			}
			s.i++
			s.operator()
		case c == '[' || c == '{':
			s.push(c)
			s.i++
			s.operator()
		case c == ')':
			top := s.close(markParen, markControl)
			s.i++
			if top == markControl {
				s.operator()
			} else {
				s.value()
			}
		case c == ']':
			s.close(markBracket, 0)
			s.i++
			s.value()
		case c == '}':
			top := s.close(markBrace, markTemplate)
			s.i++
			if top == markTemplate {
				s.template(s.i)
			} else {
				// a block or an object literal ended: both a regexp and a division may follow
				s.operator()
				s.ambiguous = true
			}
		case c == '.' && s.at(1) == '.' && s.at(2) == '.':
			s.i += 3
			s.operator()
		case c == '.' && isDigit(s.at(1)):
			s.i = s.numberEnd(s.i + 1)
			s.value()
		case c == '.' || (c == '?' && s.at(1) == '.' && !isDigit(s.at(2))):
			if c == '?' {
				s.i++
			}
			s.i++
			s.operator()
			s.afterDot = true
		case (c == '+' && s.at(1) == '+') || (c == '-' && s.at(1) == '-'):
			s.i += 2
			s.value()
		case isDigit(c):
			s.i = s.numberEnd(s.i)
			s.value()
		case isIdentByte(c):
			s.word()
		default:
			s.i++
			s.operator()
		}
	}
	return nil
}

func (s *scanner) at(offset int) byte {
	if s.i+offset < len(s.src) {
		return s.src[s.i+offset]
	}
	return 0
}

func (s *scanner) push(mark byte) {
	s.stack = append(s.stack, mark)
	s.note(0)
}

// note records the depth reached with extra levels on top of the stack.
func (s *scanner) note(extra int) {
	if d := len(s.stack) + extra; d > s.deepest {
		s.deepest = d
	}
}

// close pops the nearest opener matching one of the two marks and everything
// above it, which resynchronises after a stray opener; a closer with no match,
// or whose match lies beyond a template substitution, is ignored. It returns the
// popped mark or 0.
func (s *scanner) close(a, b byte) byte {
	for j := len(s.stack) - 1; j >= 0; j-- {
		m := s.stack[j]
		if m == a || (b != 0 && m == b) {
			s.stack = s.stack[:j]
			return m
		}
		if m == markTemplate {
			return 0
		}
	}
	return 0
}

func (s *scanner) operator() {
	s.regexOK, s.ambiguous, s.afterDot, s.control = true, false, false, false
}

func (s *scanner) value() {
	s.regexOK, s.ambiguous, s.afterDot, s.control = false, false, false, false
}

func (s *scanner) word() {
	start := s.i
	for s.i < len(s.src) {
		c := s.src[s.i]
		if c < utf8.RuneSelf {
			if !isIdentByte(c) && !isDigit(c) {
				break
			}
			s.i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s.src[s.i:])
		if r == '\u2028' || r == '\u2029' || unicode.IsSpace(r) || r == '\ufeff' {
			break
		}
		s.i += size
	}
	if s.i == start {
		s.i++ // not reached for valid input; guarantees progress
	}
	if s.afterDot {
		s.value()
		return
	}
	switch s.src[start:s.i] {
	case "if", "while", "for", "with":
		s.operator()
		s.control = true
	case "await":
		wasControl := s.control // for await (...)
		s.operator()
		s.ambiguous = true
		s.control = wasControl
	case "of", "yield":
		s.operator()
		s.ambiguous = true
	case "return", "typeof", "case", "in", "new", "delete", "void", "throw", "instanceof", "do", "else", "extends":
		s.operator()
	default:
		s.value()
	}
}

// regexp scans a literal body after its "/" and returns the index after its flags,
// or false if the line ends first (goja rejects that, so nothing is recorded).
// With countAll, for a "/" that may be a division, every opener counts and none is released.
func (s *scanner) regexp(from int, countAll bool) (int, bool) {
	groups, class, classLevels, deepest := 0, false, 0, 0
	for j := from; j < len(s.src); {
		c := s.src[j]
		if isLineTerminatorAt(s.src, j) {
			return 0, false
		}
		switch {
		case c == '\\':
			j++
			if j >= len(s.src) || isLineTerminatorAt(s.src, j) {
				return 0, false
			}
			j += runeLen(s.src, j)
			continue
		case class:
			if c == ']' {
				class = false
				if countAll {
					groups += classLevels
				}
				classLevels = 0
			} else if c == '[' || (countAll && (c == '(' || c == '{')) {
				classLevels++
			}
		case c == '[':
			class, classLevels = true, 1
		case c == '(' || (countAll && c == '{'):
			groups++
		case c == ')':
			if groups > 0 && !countAll {
				groups--
			}
		case c == '/':
			j++
			for j < len(s.src) && (isIdentByte(s.src[j]) || isDigit(s.src[j])) {
				j++ // flags
			}
			s.note(deepest)
			return j, true
		}
		if groups+classLevels > deepest {
			deepest = groups + classLevels
		}
		j++
	}
	return 0, false
}

// stringEnd returns the index after a string closed by quote, or the index of the
// line terminator that leaves it unterminated. U+2028 and U+2029 may appear raw
// in a string; a backslash before a line terminator continues it.
func (s *scanner) stringEnd(from int, quote byte) int {
	for j := from; j < len(s.src); {
		c := s.src[j]
		switch {
		case c == quote:
			return j + 1
		case c == '\n' || c == '\r':
			return j
		case c == '\\':
			j++
			if j < len(s.src) && s.src[j] == '\r' && j+1 < len(s.src) && s.src[j+1] == '\n' {
				j += 2
				continue
			}
			if j < len(s.src) {
				j += runeLen(s.src, j)
			}
			continue
		}
		j++
	}
	return len(s.src)
}

// template scans template text from "from" up to the closing backtick, after
// which a value has been read, or up to "${", which opens a substitution level.
func (s *scanner) template(from int) {
	for j := from; j < len(s.src); {
		c := s.src[j]
		switch {
		case c == '\\':
			j++
			if j < len(s.src) {
				j += runeLen(s.src, j)
			}
			continue
		case c == '`':
			s.i = j + 1
			s.value()
			return
		case c == '$' && j+1 < len(s.src) && s.src[j+1] == '{':
			s.push(markTemplate)
			s.i = j + 2
			s.operator()
			return
		}
		j++
	}
	s.i = len(s.src)
}

// lineEnd returns the index of the next line terminator (goja's: \n, \r, U+2028,
// U+2029) at or after from.
func (s *scanner) lineEnd(from int) int {
	for j := from; j < len(s.src); j++ {
		if isLineTerminatorAt(s.src, j) {
			return j
		}
	}
	return len(s.src)
}

func (s *scanner) blockCommentEnd(from int) int {
	for j := from; j+1 < len(s.src); j++ {
		if s.src[j] == '*' && s.src[j+1] == '/' {
			return j + 2
		}
	}
	return len(s.src)
}

// numberEnd covers decimal, hex, octal, binary, exponent, separator and bigint
// forms; a sign after an exponent is read as an operator, which changes nothing.
func (s *scanner) numberEnd(from int) int {
	j := from
	for j < len(s.src) && (isDigit(s.src[j]) || isIdentByte(s.src[j]) || s.src[j] == '.') {
		j++
	}
	return j
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isIdentByte covers the ASCII identifier characters plus "\" of a unicode
// escape and "#" of a private name.
func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c == '\\' || c == '#' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isLineTerminatorAt(src string, j int) bool {
	switch src[j] {
	case '\n', '\r':
		return true
	case 0xE2:
		// U+2028 and U+2029 are E2 80 A8 and E2 80 A9
		return j+2 < len(src) && src[j+1] == 0x80 && (src[j+2] == 0xA8 || src[j+2] == 0xA9)
	}
	return false
}

func runeLen(src string, j int) int {
	if src[j] < utf8.RuneSelf {
		return 1
	}
	_, size := utf8.DecodeRuneInString(src[j:])
	return size
}
