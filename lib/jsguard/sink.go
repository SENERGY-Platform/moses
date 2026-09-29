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

import "errors"

// ErrSinkReentry refuses a moses api or console call made from inside another
// one's value conversion, e.g. a getter that calls send while its holder is
// converted, so conversions never nest and multiply the Go stack.
var ErrSinkReentry = errors.New("a moses api call from inside another value conversion is not allowed")

// SinkGuard marks a value conversion in progress. It holds no lock: every run of
// one environment or world is serialised by that owner's mutex.
type SinkGuard struct{ busy bool }

// Enter reports false when a conversion is already in progress; otherwise the
// caller must call Leave when its conversion ends.
func (this *SinkGuard) Enter() bool {
	if this.busy {
		return false
	}
	this.busy = true
	return true
}

func (this *SinkGuard) Leave() { this.busy = false }
