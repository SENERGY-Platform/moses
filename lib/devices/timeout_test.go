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

package devices

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/platformhttp"
)

// shortClientTimeout is the read timeout of these tests and shortWriteDeadline
// the deadline of their writes, well above it; callGiveUp is how long a test
// waits before it calls the call hung, far above both.
const (
	shortClientTimeout = 200 * time.Millisecond
	shortWriteDeadline = 3 * shortClientTimeout
	callGiveUp         = 5 * time.Second
)

// hangingPlatform accepts every request and never answers; requests receives
// each one as it arrives. The handler ends when the client gives up.
func hangingPlatform(t *testing.T) (*httptest.Server, chan struct{}) {
	t.Helper()
	release := make(chan struct{})
	requests := make(chan struct{}, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	return server, requests
}

// hangingCalls are the catalog's direct requests: the check's read of the
// repository and the device-manager writes a save makes.
func hangingCalls(catalog *Catalog) map[string]func(ctx context.Context) error {
	return map[string]func(ctx context.Context) error{
		"check": func(ctx context.Context) error {
			_, err := catalog.CheckReferences(ctx, "Bearer t", []AssetReference{{Path: "a0", DeviceTypeId: "dt-1"}})
			return err
		},
		"create": func(ctx context.Context) error {
			_, err := catalog.CreateDevice(ctx, "Bearer t", "dt-1", "n")
			return err
		},
		"delete": func(ctx context.Context) error {
			return catalog.DeleteDevice(ctx, "Bearer t", "urn:device:x")
		},
		"rename": func(ctx context.Context) error {
			return catalog.RenameDevice(ctx, "Bearer t", "urn:device:x", "n")
		},
	}
}

func hangingCatalog(t *testing.T) (*Catalog, chan struct{}) {
	t.Helper()
	server, requests := hangingPlatform(t)
	catalog := NewCatalog(server.URL, server.URL, "moses", platformhttp.NewClients(shortClientTimeout))
	catalog.repo = &fakeRegistry{}
	return catalog, requests
}

// waitFor fails the test when result does not arrive within limit.
func waitFor(t *testing.T, name string, result chan error, limit time.Duration) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(limit):
		t.Fatalf("%s: still waiting for the platform after %v", name, limit)
		return nil
	}
}

// Against a platform that accepts the connection and never answers, the check's
// read ends at the read timeout even without a deadline of its own, and a write
// at the deadline of its context, not at the read timeout.
func TestAHangingPlatformEndsAReadAtTheTimeoutAndAWriteAtItsDeadline(t *testing.T) {
	catalog, _ := hangingCatalog(t)
	for name, call := range hangingCalls(catalog) {
		ctx, bound := context.Background(), shortClientTimeout
		cancel := context.CancelFunc(func() {})
		if name != "check" {
			bound = shortWriteDeadline
			ctx, cancel = context.WithTimeout(ctx, bound)
		}
		result := make(chan error, 1)
		started := time.Now()
		go func() { result <- call(ctx) }()
		err := waitFor(t, name, result, callGiveUp)
		cancel()
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Errorf("%s: expected a timeout error, got %v", name, err)
		}
		elapsed := time.Since(started)
		if elapsed < bound {
			t.Errorf("%s: gave up after %v, before its bound of %v", name, elapsed, bound)
		}
		if name == "check" && elapsed >= shortWriteDeadline {
			t.Errorf("check: the read took %v, past the read timeout of %v", elapsed, shortClientTimeout)
		}
	}
}

// A write slower than the read timeout completes: the device may already exist on the platform.
func TestAWriteSlowerThanTheReadTimeoutCompletes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * shortClientTimeout)
		_, _ = w.Write([]byte(`{"id":"urn:device:created","device_type_id":"dt-1"}`))
	}))
	t.Cleanup(server.Close)
	catalog := NewCatalog(server.URL, server.URL, "moses", platformhttp.NewClients(shortClientTimeout))
	device, err := catalog.CreateDevice(context.Background(), "Bearer t", "dt-1", "n")
	if err != nil || device.Id != "urn:device:created" {
		t.Fatalf("expected the slow create to complete, got %+v, %v", device, err)
	}
}

// A caller that goes away ends the outbound call with it, long before the timeout.
func TestCancellingTheCallerAbortsTheCall(t *testing.T) {
	server, requests := hangingPlatform(t)
	catalog := NewCatalog(server.URL, server.URL, "moses", platformhttp.NewClients(time.Hour))
	catalog.repo = &fakeRegistry{}
	for name, call := range hangingCalls(catalog) {
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- call(ctx) }()
		select {
		case <-requests:
		case <-time.After(callGiveUp):
			cancel()
			t.Fatalf("%s: the call never reached the platform", name)
		}
		cancel()
		if err := waitFor(t, name, result, time.Second); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: expected the cancellation, got %v", name, err)
		}
	}
}

// A Catalog built without clients still gets bounded ones.
func TestACatalogWithoutClientsUsesTheDefaults(t *testing.T) {
	for name, catalog := range map[string]*Catalog{
		"NewCatalog without clients": NewCatalog("http://repo", "http://manager", "moses", platformhttp.Clients{}),
		"zero value":                 {},
	} {
		if got := catalog.clients.Reads().Timeout; got != platformhttp.DefaultTimeout {
			t.Errorf("%s: expected the default read timeout %v, got %v", name, platformhttp.DefaultTimeout, got)
		}
		if got := catalog.clients.Writes().Timeout; got != platformhttp.WriteTimeout {
			t.Errorf("%s: expected the write bound %v, got %v", name, platformhttp.WriteTimeout, got)
		}
	}
}
