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

package domain

import (
	"regexp"
	"sort"

	"github.com/SENERGY-Platform/moses/lib/state"
)

// A legacy device carried its physics in change routines on their own intervals,
// while its sensor services read the resulting state on theirs. The new model has
// a source per channel, so a routine is attached to the channel that publishes
// the state it writes, and the channel keeps its own publish interval.
//
// The state key is what connects the two, and it is read out of the javascript
// rather than declared anywhere. That is a heuristic, so an unmatched routine is
// reported instead of guessed at, and the migration is a dry run by default.
// statePattern matches one scoped state access. The scope is part of the
// expression on purpose: a script that touches moses.device.state and
// moses.room.state under the same key is otherwise indistinguishable.
//
// A script reaching state through moses.room.getDevice(x) does not match and its
// routine comes back unmatched, which is the safe direction.
var statePattern = regexp.MustCompile(`moses\s*\.\s*(device|asset|room|zone|world|environment)\s*\.\s*state\s*\.\s*(set|get)\s*\(\s*["']([^"']+)["']`)

// deviceStateKeys returns the keys a script writes to and reads from the device
// scope, sorted and without duplicates. Only the device scope counts: a routine
// writing room or world state has no asset channel to attach to.
func deviceStateKeys(code string) (written []string, read []string) {
	writtenSet, readSet := map[string]bool{}, map[string]bool{}
	for _, match := range statePattern.FindAllStringSubmatch(code, -1) {
		scope, operation, key := match[1], match[2], match[3]
		if scope != "device" && scope != "asset" {
			continue
		}
		if operation == "set" {
			writtenSet[key] = true
		} else {
			readSet[key] = true
		}
	}
	return sortedKeys(writtenSet), sortedKeys(readSet)
}

func sortedKeys(in map[string]bool) []string {
	result := make([]string, 0, len(in))
	for key := range in {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

// attachRoutines gives each change routine the channel that publishes the state
// it writes, and returns the routines that found none.
//
// A routine that writes two states is attached once, to the channel of its
// alphabetically first key; the other channel stays a plain reader. That is the
// only faithful split: the two values come out of one draw, and running the
// physics twice would decouple them.
func attachRoutines(channels []Channel, routines map[string]state.ChangeRoutine) (unmatched []string) {
	readers := map[string][]int{}
	for index, channel := range channels {
		if channel.Source.Kind != SourceScript || channel.Source.Script == nil {
			continue
		}
		_, read := deviceStateKeys(channel.Source.Script.Code)
		for _, key := range read {
			readers[key] = append(readers[key], index)
		}
	}

	taken := map[int]bool{}
	keys := make([]string, 0, len(routines))
	for key := range routines {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		routine := routines[key]
		written, _ := deviceStateKeys(routine.Code)
		attached := false
		for _, stateKey := range written {
			for _, index := range readers[stateKey] {
				if taken[index] {
					continue
				}
				taken[index] = true
				//the physics first, then the reading the legacy service did: one
				//script, run on the routine's interval, publishing on the
				//channel's own
				channels[index].Source.Script.Code = routine.Code + "\n" + channels[index].Source.Script.Code
				channels[index].Source.IntervalSeconds = routine.Interval
				attached = true
				break
			}
			if attached {
				break
			}
		}
		if !attached {
			unmatched = append(unmatched, key)
		}
	}
	return unmatched
}
