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

import "encoding/binary"

// register is the in-flight store: a memory-mapped file (register_linux.go) or a
// no-op off linux and when disabled (register_stub.go).
type register interface {
	recover() (inflight []InFlight, cleanShutdown bool)
	reset()
	mark(index int, goroutine uint64, environment string, channel string)
	clear(index int)
	markClean()
	close()
}

// headerSize is the bytes before the first slot; byte 0 is the clean-shutdown flag.
const headerSize = 8

// registerBytes is the whole mapping size.
const registerBytes = headerSize + SlotCount*slotSize

// A slot is [0] occupied, [1:9] goroutine id, [9:11] env length + env bytes, then
// channel length + channel bytes. The env id is stored whole (the api bounds it);
// the channel is informational and may be truncated.
const (
	goroutineOffset = 1
	envLenOffset    = 9
	envOffset       = 11
	envCapacity     = MaxEnvironmentIdBytes
)

// MaxEnvironmentIdBytes is the most an environment id may hold so it always fits a
// slot exactly; the api refuses a longer one.
const MaxEnvironmentIdBytes = 256

// encodeEntry writes the run's goroutine, environment and channel into a slot,
// truncating only the informational channel.
func encodeEntry(slot []byte, goroutine uint64, environment string, channel string) {
	binary.LittleEndian.PutUint64(slot[goroutineOffset:envLenOffset], goroutine)
	env := environment
	if len(env) > envCapacity {
		env = env[:envCapacity]
	}
	binary.LittleEndian.PutUint16(slot[envLenOffset:envOffset], uint16(len(env)))
	n := envOffset + copy(slot[envOffset:envOffset+envCapacity], env)
	chanRoom := len(slot) - n - 2
	if chanRoom < 0 {
		chanRoom = 0
	}
	ch := channel
	if len(ch) > chanRoom {
		ch = ch[:chanRoom]
	}
	binary.LittleEndian.PutUint16(slot[n:n+2], uint16(len(ch)))
	copy(slot[n+2:], ch)
}

// decodeEntry parses a slot, reporting false for a torn write whose lengths do not
// fit the slot exactly, which is then never blamed on an environment.
func decodeEntry(slot []byte) (InFlight, bool) {
	goroutine := binary.LittleEndian.Uint64(slot[goroutineOffset:envLenOffset])
	envLen := int(binary.LittleEndian.Uint16(slot[envLenOffset:envOffset]))
	if envLen > envCapacity || envOffset+envLen+2 > len(slot) {
		return InFlight{}, false
	}
	env := string(slot[envOffset : envOffset+envLen])
	chanStart := envOffset + envLen
	chanLen := int(binary.LittleEndian.Uint16(slot[chanStart : chanStart+2]))
	if chanStart+2+chanLen > len(slot) {
		return InFlight{}, false
	}
	return InFlight{Goroutine: goroutine, Environment: env, Channel: string(slot[chanStart+2 : chanStart+2+chanLen])}, true
}

// noopRegister is the disabled brake: it recovers as a clean shutdown and never
// quarantines.
type noopRegister struct{}

func (noopRegister) recover() ([]InFlight, bool)      { return nil, true }
func (noopRegister) reset()                           {}
func (noopRegister) mark(int, uint64, string, string) {}
func (noopRegister) clear(int)                        {}
func (noopRegister) markClean()                       {}
func (noopRegister) close()                           {}
