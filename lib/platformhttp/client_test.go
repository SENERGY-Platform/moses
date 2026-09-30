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

package platformhttp

import (
	"testing"
	"time"
)

// An unset or negative timeout must not become a client without any bound.
func TestNewNeverReturnsAnUnboundedClient(t *testing.T) {
	for _, configured := range []time.Duration{0, -time.Second} {
		if got := New(configured).Timeout; got != DefaultTimeout {
			t.Errorf("timeout %v: expected the default %v, got %v", configured, DefaultTimeout, got)
		}
	}
	if got := New(250 * time.Millisecond).Timeout; got != 250*time.Millisecond {
		t.Errorf("expected the configured 250ms, got %v", got)
	}
}

// Reads and writes get different bounds, and an unset client is never unbounded.
func TestTheClientsBoundReadsAndWritesApart(t *testing.T) {
	clients := NewClients(250 * time.Millisecond)
	if got := clients.Reads().Timeout; got != 250*time.Millisecond {
		t.Errorf("reads: expected the configured 250ms, got %v", got)
	}
	if got := clients.Writes().Timeout; got != WriteTimeout {
		t.Errorf("writes: expected the write bound %v, got %v", WriteTimeout, got)
	}
	unset := Clients{}
	if got := unset.Reads().Timeout; got != DefaultTimeout {
		t.Errorf("unset reads: expected the default %v, got %v", DefaultTimeout, got)
	}
	if got := unset.Writes().Timeout; got != WriteTimeout {
		t.Errorf("unset writes: expected the write bound %v, got %v", WriteTimeout, got)
	}
}
