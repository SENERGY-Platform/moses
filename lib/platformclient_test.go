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

package lib

import (
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/platformhttp"
)

// The clients the service shares: reads take the configured timeout, and an
// unset one becomes the default instead of no bound; writes take the write bound.
func TestThePlatformClientsTakeTheConfiguredTimeout(t *testing.T) {
	configured := newPlatformClients(3 * time.Second)
	if got := configured.Read.Timeout; got != 3*time.Second {
		t.Errorf("expected the configured 3s for reads, got %v", got)
	}
	if got := configured.Write.Timeout; got != platformhttp.WriteTimeout {
		t.Errorf("expected the write bound %v for writes, got %v", platformhttp.WriteTimeout, got)
	}
	if got := newPlatformClients(0).Read.Timeout; got != platformhttp.DefaultTimeout {
		t.Errorf("expected the default %v for an unset timeout, got %v", platformhttp.DefaultTimeout, got)
	}
}
