//go:build linux

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

package crashbrake

import (
	"fmt"
	"os"
	"syscall"
)

// mmapRegister is the register backed by a memory-mapped file. It is mapped
// MAP_SHARED, so a store into the byte slice lands in the file's page cache and
// the kernel writes it back even after the process dies - which is what lets the
// next boot read what was in flight at a crash, without a syscall per run.
type mmapRegister struct {
	file *os.File
	data []byte
}

// openRegister maps path, creating and sizing it on first use, and returns a
// register still holding the previous run's contents until reset is called.
func openRegister(path string) (register, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("crash brake register open: %w", err)
	}
	if err := file.Truncate(registerBytes); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("crash brake register size: %w", err)
	}
	data, err := syscall.Mmap(int(file.Fd()), 0, registerBytes, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("crash brake register mmap: %w", err)
	}
	return &mmapRegister{file: file, data: data}, nil
}

func (this *mmapRegister) slot(index int) []byte {
	start := headerSize + index*slotSize
	return this.data[start : start+slotSize]
}

func (this *mmapRegister) recover() ([]InFlight, bool) {
	clean := this.data[0] == 1
	var inflight []InFlight
	for i := 0; i < SlotCount; i++ {
		slot := this.slot(i)
		if slot[0] != 1 {
			continue
		}
		//an entry that does not decode exactly is a torn write, dropped rather than
		//blamed on an environment
		if entry, ok := decodeEntry(slot); ok {
			inflight = append(inflight, entry)
		}
	}
	return inflight, clean
}

func (this *mmapRegister) reset() {
	for i := range this.data {
		this.data[i] = 0
	}
}

func (this *mmapRegister) mark(index int, goroutine uint64, environment string, channel string) {
	slot := this.slot(index)
	//the fields are written before the occupied flag, so a reader that sees the
	//flag sees a whole entry: the single-byte flag store publishes the bytes before it
	encodeEntry(slot, goroutine, environment, channel)
	slot[0] = 1
}

func (this *mmapRegister) clear(index int) {
	this.slot(index)[0] = 0
}

func (this *mmapRegister) markClean() {
	for i := 0; i < SlotCount; i++ {
		this.slot(i)[0] = 0
	}
	this.data[0] = 1
}

// close releases the file descriptor but keeps the mapping, because an untracked
// run may still release its slot after Close and a write to unmapped memory is a SIGSEGV.
func (this *mmapRegister) close() {
	_ = this.file.Close()
}
