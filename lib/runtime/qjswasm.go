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

// SPIKE (SNRGY-4817 variant C): a binary patch of the qjs.wasm that fastschema/qjs
// embeds. The shipped build puts the 64 KiB shadow stack above the data section
// (its --stack-first flag never reaches the target), disables QuickJS's stack
// check under WASI and counts 8 bytes per allocation against the memory limit,
// so a recursion ~250 frames deep silently corrupts static data. The patch caps
// linear memory and traps any stack-pointer write below the stack's floor. The
// stack cannot be enlarged here: dlmalloc's init has the heap base folded into
// several constants, which only a rebuild with --stack-first can move.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// qjsWasmLayout is the layout of qjs v0.0.6's qjs.wasm, read from its global,
// data and code sections; the patch refuses a binary that does not match it.
const (
	qjsStackTop      = 165408 // initial __stack_pointer and __heap_base
	qjsStackBottom   = 165408 - 65536
	qjsWasmPageBytes = 65536
)

type qjsPatch struct {
	maxPages   uint32 // linear memory cap, 0 keeps the module unbounded
	checkStack bool   // trap on a stack-pointer write below the floor
	interrupt  bool   // trap at a loop header once the word at qjsInterruptAddr is set
}

// qjsInterruptAddr is a word below the first data segment (1024), which neither
// the data nor the heap ever uses; a run's timer sets it to stop the instance.
const qjsInterruptAddr = 8

// qjsInterruptCheck is "if (i32.load offset=8 (i32.const 0)) unreachable".
var qjsInterruptCheck = []byte{0x41, 0x00, 0x28, 0x02, qjsInterruptAddr, 0x04, 0x40, 0x00, 0x0b}

func wasmReadULEB(b []byte, i int) (uint64, int, error) {
	var r uint64
	var s uint
	for {
		if i >= len(b) {
			return 0, i, errors.New("truncated leb128")
		}
		x := b[i]
		i++
		r |= uint64(x&0x7f) << s
		s += 7
		if x < 0x80 {
			return r, i, nil
		}
		if s > 63 {
			return 0, i, errors.New("leb128 too long")
		}
	}
}

func wasmReadSLEB(b []byte, i int) (int64, int, error) {
	var r int64
	var s uint
	for {
		if i >= len(b) {
			return 0, i, errors.New("truncated leb128")
		}
		x := b[i]
		i++
		r |= int64(x&0x7f) << s
		s += 7
		if x < 0x80 {
			if s < 64 && x&0x40 != 0 {
				r |= -1 << s
			}
			return r, i, nil
		}
		if s > 63 {
			return 0, i, errors.New("leb128 too long")
		}
	}
}

func wasmULEB(v uint64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			out = append(out, b|0x80)
			continue
		}
		return append(out, b)
	}
}

func wasmSLEB(v int64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}

type wasmSection struct {
	id   byte
	body []byte
}

func wasmSections(module []byte) ([]wasmSection, error) {
	if len(module) < 8 || !bytes.Equal(module[:8], []byte{0, 'a', 's', 'm', 1, 0, 0, 0}) {
		return nil, errors.New("not a wasm 1.0 module")
	}
	var sections []wasmSection
	for i := 8; i < len(module); {
		id := module[i]
		size, next, err := wasmReadULEB(module, i+1)
		if err != nil {
			return nil, err
		}
		end := next + int(size)
		if end > len(module) {
			return nil, errors.New("section past the end of the module")
		}
		sections = append(sections, wasmSection{id: id, body: module[next:end]})
		i = end
	}
	return sections, nil
}

// wasmSkipInstruction returns the offset after the immediates of the instruction
// at i, for the MVP, sign-extension, saturating and bulk-memory opcodes the
// target_features section of qjs.wasm declares.
func wasmSkipInstruction(code []byte, i int) (int, error) {
	op := code[i]
	i++
	var err error
	uleb := func() {
		if err == nil {
			_, i, err = wasmReadULEB(code, i)
		}
	}
	switch {
	case op == 0x02 || op == 0x03 || op == 0x04:
		//an inline block type is the empty type 0x40 or a single-byte value type
		//(0x6f..0x7f); anything else is a type index as a signed LEB, whose first
		//byte for an index >= 64 is >= 0x80 and must not be read as one byte
		if code[i] == 0x40 || (code[i] >= 0x6f && code[i] <= 0x7f) {
			i++
		} else {
			_, i, err = wasmReadSLEB(code, i)
		}
	case op == 0x0c || op == 0x0d || op == 0x10 || (op >= 0x20 && op <= 0x26) || op == 0xd2:
		uleb()
	case op == 0x0e:
		var n uint64
		n, i, err = wasmReadULEB(code, i)
		for k := uint64(0); k <= n && err == nil; k++ {
			uleb()
		}
	case op == 0x11:
		uleb()
		uleb()
	case op == 0x1c:
		var n uint64
		n, i, err = wasmReadULEB(code, i)
		i += int(n)
	case op >= 0x28 && op <= 0x3e:
		uleb()
		uleb()
	case op == 0x3f || op == 0x40 || op == 0xd0:
		i++
	case op == 0x41:
		_, i, err = wasmReadSLEB(code, i)
	case op == 0x42:
		_, i, err = wasmReadSLEB(code, i)
	case op == 0x43:
		i += 4
	case op == 0x44:
		i += 8
	case op <= 0x01 || op == 0x05 || op == 0x0b || op == 0x0f || op == 0x1a || op == 0x1b || (op >= 0x45 && op <= 0xc4) || op == 0xd1:
	case op == 0xfc:
		var sub uint64
		sub, i, err = wasmReadULEB(code, i)
		switch {
		case sub <= 7:
		case sub == 8:
			uleb()
			i++
		case sub == 9 || sub == 13 || (sub >= 15 && sub <= 17):
			uleb()
		case sub == 10:
			i += 2
		case sub == 11:
			i++
		case sub == 12 || sub == 14:
			uleb()
			uleb()
		default:
			return i, fmt.Errorf("unknown 0xfc opcode %d", sub)
		}
	default:
		return i, fmt.Errorf("unknown opcode %#x", op)
	}
	if err != nil {
		return i, err
	}
	if i > len(code) {
		return i, errors.New("instruction past the end of the body")
	}
	return i, nil
}

// patchQJSWasm rewrites module as p asks. The stack move relies on the layout
// constants above, which it checks against the global section first.
func patchQJSWasm(module []byte, p qjsPatch) ([]byte, error) {
	sections, err := wasmSections(module)
	if err != nil {
		return nil, err
	}
	stackTop := uint32(qjsStackTop)
	floor := uint32(qjsStackBottom)
	minPages := (stackTop + qjsWasmPageBytes - 1) / qjsWasmPageBytes
	var importedFuncs, definedFuncs uint64
	var checkType int64 = -1
	var typeCount uint64
	for _, s := range sections {
		switch s.id {
		case 1:
			n, j, err := wasmReadULEB(s.body, 0)
			if err != nil {
				return nil, err
			}
			typeCount = n
			for k := uint64(0); k < n; k++ {
				if s.body[j] != 0x60 {
					return nil, errors.New("unexpected type form")
				}
				start := j
				params, j2, _ := wasmReadULEB(s.body, j+1)
				j = j2 + int(params)
				results, j3, _ := wasmReadULEB(s.body, j)
				j = j3 + int(results)
				if bytes.Equal(s.body[start:j], []byte{0x60, 1, 0x7f, 0}) && checkType < 0 {
					checkType = int64(k)
				}
			}
		case 2:
			n, j, err := wasmReadULEB(s.body, 0)
			if err != nil {
				return nil, err
			}
			for k := uint64(0); k < n; k++ {
				for f := 0; f < 2; f++ {
					l, j2, _ := wasmReadULEB(s.body, j)
					j = j2 + int(l)
				}
				kind := s.body[j]
				j++
				switch kind {
				case 0:
					importedFuncs++
					_, j, _ = wasmReadULEB(s.body, j)
				case 1:
					j += 1
					flags := s.body[j]
					j++
					_, j, _ = wasmReadULEB(s.body, j)
					if flags&1 != 0 {
						_, j, _ = wasmReadULEB(s.body, j)
					}
				case 2:
					return nil, errors.New("imported memory is not supported")
				case 3:
					j += 2
				}
			}
		case 3:
			definedFuncs, _, err = wasmReadULEB(s.body, 0)
			if err != nil {
				return nil, err
			}
		case 6:
			//exactly one mutable i32 global, initialised to the known stack top
			if !bytes.Equal(s.body, append(append([]byte{1, 0x7f, 1, 0x41}, wasmSLEB(qjsStackTop)...), 0x0b)) {
				return nil, errors.New("the global section is not the one of qjs v0.0.6")
			}
		}
	}
	checkFunc := importedFuncs + definedFuncs

	var out bytes.Buffer
	out.Write(module[:8])
	emit := func(id byte, body []byte) {
		out.WriteByte(id)
		out.Write(wasmULEB(uint64(len(body))))
		out.Write(body)
	}
	for _, s := range sections {
		switch {
		case s.id == 0:
			//DWARF offsets would point into rewritten code
			if p.checkStack || p.interrupt {
				continue
			}
			emit(s.id, s.body)
		case s.id == 1 && p.checkStack && checkType < 0:
			body := append(wasmULEB(typeCount+1), s.body[len(wasmULEB(typeCount)):]...)
			body = append(body, 0x60, 1, 0x7f, 0)
			checkType = int64(typeCount)
			emit(1, body)
		case s.id == 3 && p.checkStack:
			n := definedFuncs
			body := append(wasmULEB(n+1), s.body[len(wasmULEB(n)):]...)
			if checkType < 0 {
				checkType = int64(typeCount)
			}
			body = append(body, wasmULEB(uint64(checkType))...)
			emit(3, body)
		case s.id == 5:
			n, j, _ := wasmReadULEB(s.body, 0)
			if n != 1 {
				return nil, errors.New("expected one memory")
			}
			flags := s.body[j]
			origMin, _, _ := wasmReadULEB(s.body, j+1)
			if flags != 0 {
				return nil, errors.New("the memory already has a maximum")
			}
			pages := uint64(minPages)
			if origMin > pages {
				pages = origMin
			}
			body := []byte{1}
			if p.maxPages > 0 {
				if uint64(p.maxPages) < pages {
					return nil, errors.New("the memory cap is below the stack")
				}
				body = append(body, 1)
				body = append(body, wasmULEB(pages)...)
				body = append(body, wasmULEB(uint64(p.maxPages))...)
			} else {
				body = append(body, 0)
				body = append(body, wasmULEB(pages)...)
			}
			emit(5, body)
		case s.id == 10 && (p.checkStack || p.interrupt):
			body, err := patchCode(s.body, p, uint32(checkFunc))
			if err != nil {
				return nil, err
			}
			if p.checkStack {
				n, _, _ := wasmReadULEB(body, 0)
				body = append(wasmULEB(n+1), body[len(wasmULEB(n)):]...)
				//(param i32): trap below the floor plus a margin for leaf frames, which
				//write below the stack pointer without storing it
				fn := []byte{0, 0x20, 0, 0x41}
				fn = append(fn, wasmSLEB(int64(floor+8192))...)
				fn = append(fn, 0x49, 0x04, 0x40, 0x00, 0x0b, 0x20, 0, 0x24, 0, 0x0b)
				body = append(body, wasmULEB(uint64(len(fn)))...)
				body = append(body, fn...)
			}
			emit(10, body)
		default:
			emit(s.id, s.body)
		}
	}
	return out.Bytes(), nil
}

// patchCode rewrites every "global.set 0" (the stack pointer) into a call of
// the check function, which stores the value only once it is above the floor,
// and starts every loop body with the interrupt check.
func patchCode(section []byte, p qjsPatch, checkFunc uint32) ([]byte, error) {
	n, j, err := wasmReadULEB(section, 0)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Write(wasmULEB(n))
	call := append([]byte{0x10}, wasmULEB(uint64(checkFunc))...)
	for f := uint64(0); f < n; f++ {
		size, start, err := wasmReadULEB(section, j)
		if err != nil {
			return nil, err
		}
		end := start + int(size)
		if end > len(section) {
			return nil, errors.New("function body past the end of the code section")
		}
		body := section[start:end]
		j = end
		groups, k, err := wasmReadULEB(body, 0)
		if err != nil {
			return nil, err
		}
		for g := uint64(0); g < groups; g++ {
			if _, k, err = wasmReadULEB(body, k); err != nil {
				return nil, err
			}
			k++
		}
		var fn bytes.Buffer
		fn.Write(body[:k])
		for k < len(body) {
			next, err := wasmSkipInstruction(body, k)
			if err != nil {
				return nil, fmt.Errorf("function %d at %d: %w", f, k, err)
			}
			switch {
			case p.checkStack && body[k] == 0x24 && next == k+2 && body[k+1] == 0:
				fn.Write(call)
			case p.interrupt && body[k] == 0x03:
				fn.Write(body[k:next])
				fn.Write(qjsInterruptCheck)
			default:
				fn.Write(body[k:next])
			}
			k = next
		}
		out.Write(wasmULEB(uint64(fn.Len())))
		out.Write(fn.Bytes())
	}
	return out.Bytes(), nil
}

// qjsOriginalWasm reads the qjs.wasm the module ships, since the package does not
// export its embedded copy; a production build would vendor a rebuilt binary.
func qjsOriginalWasm() ([]byte, error) {
	if path := os.Getenv("MOSES_QJS_WASM"); path != "" {
		return os.ReadFile(path)
	}
	cache := os.Getenv("GOMODCACHE")
	if cache == "" {
		gopath := os.Getenv("GOPATH")
		if gopath == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			gopath = filepath.Join(home, "go")
		}
		cache = filepath.Join(gopath, "pkg", "mod")
	}
	return os.ReadFile(filepath.Join(cache, "github.com", "fastschema", "qjs@v0.0.6", "qjs.wasm"))
}
