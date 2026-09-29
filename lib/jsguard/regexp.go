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
	"time"

	"github.com/dlclark/regexp2/v2"
)

// RegexpMatchTimeout bounds one match on goja's backtracking engine, which the
// script interrupt cannot stop; a timed-out match reads as no match. It stays
// well under the default JS_TIMEOUT of 2 s, so a run overshoots by at most this.
const RegexpMatchTimeout = 250 * time.Millisecond

// Set in an init of this package because both engines' packages import it:
// regexp2 copies the default into every pattern at compile time, including the
// ones goja compiles lazily, so it has to be in place before any script compiles.
func init() {
	regexp2.DefaultMatchTimeout = RegexpMatchTimeout
}
