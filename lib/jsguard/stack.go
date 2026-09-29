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

import "runtime/debug"

// MaxGoroutineStack makes a missed native recursion a fatal "stack overflow" with a
// crash report before the pod's memory limit kills the process without one; the worst parse needs 128 MB.
const MaxGoroutineStack = 256 << 20

// LimitStack applies MaxGoroutineStack to the process.
func LimitStack() {
	debug.SetMaxStack(MaxGoroutineStack)
}
