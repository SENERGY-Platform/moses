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

func TestJSONDepthBoundary(t *testing.T) {
	at := strings.Repeat("[", MaxJSONDepth) + strings.Repeat("]", MaxJSONDepth)
	if err := JSONTooComplex(at); err != nil {
		t.Fatalf("JSON nested exactly to the limit was refused: %v", err)
	}
	over := strings.Repeat(`{"a":`, MaxJSONDepth+1) + "1" + strings.Repeat("}", MaxJSONDepth+1)
	if JSONTooComplex(over) == nil {
		t.Fatal("JSON one level past the limit was accepted")
	}
}

func TestJSONBracketsInStringsDoNotCount(t *testing.T) {
	text := `{"a": "` + strings.Repeat("[", MaxJSONDepth+1) + `\"` + strings.Repeat("{", MaxJSONDepth+1) + `"}`
	if err := JSONTooComplex(text); err != nil {
		t.Fatalf("brackets inside a JSON string were counted: %v", err)
	}
}

func TestJSONFlatAndLongIsFine(t *testing.T) {
	text := "[" + strings.Repeat("1,", 1<<20) + "1]"
	if err := JSONTooComplex(text); err != nil {
		t.Fatalf("a long flat document was refused: %v", err)
	}
}

func TestRegexpDepthBoundary(t *testing.T) {
	at := strings.Repeat("(", MaxRegexpDepth) + "a" + strings.Repeat(")", MaxRegexpDepth)
	if err := RegexpTooComplex(at); err != nil {
		t.Fatalf("a pattern nested exactly to the limit was refused: %v", err)
	}
	over := "(?=a)" + strings.Repeat("(", MaxRegexpDepth+1) + "a" + strings.Repeat(")", MaxRegexpDepth+1)
	if RegexpTooComplex(over) == nil {
		t.Fatal("a pattern one level past the limit was accepted")
	}
}

func TestRegexpEscapesAndClassesDoNotCount(t *testing.T) {
	pattern := strings.Repeat(`\(`, 300) + "[" + strings.Repeat("(", 300) + "]" + strings.Repeat(`[(]`, 300)
	if err := RegexpTooComplex(pattern); err != nil {
		t.Fatalf("escaped or class parens were counted: %v", err)
	}
}

func TestRegexpLengthBoundary(t *testing.T) {
	if err := RegexpTooComplex(strings.Repeat("a", MaxRegexpBytes)); err != nil {
		t.Fatalf("a pattern at the size limit was refused: %v", err)
	}
	if RegexpTooComplex(strings.Repeat("a", MaxRegexpBytes+1)) == nil {
		t.Fatal("a pattern past the size limit was accepted")
	}
}
