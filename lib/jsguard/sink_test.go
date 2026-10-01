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

import "testing"

// The mark of a spent run lasts until the next run starts and leaves the
// conversion guard alone.
func TestSinkGuardExpiry(t *testing.T) {
	var guard SinkGuard
	if guard.Expired() {
		t.Fatal("a new guard must not be expired")
	}
	guard.Expire()
	if !guard.Expired() {
		t.Fatal("Expire must mark the run")
	}
	if !guard.Enter() {
		t.Fatal("an expired run must not block a conversion")
	}
	guard.Leave()
	guard.StartRun()
	if guard.Expired() {
		t.Fatal("StartRun must clear the mark")
	}
}
