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

package state

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/platformhttp"
)

// shortPlatformTimeout is the configured read timeout of these tests and
// shortWriteBound their write bound, well above it; callGiveUp is how long a test
// waits before it calls the call hung, far above both.
const (
	shortPlatformTimeout = 200 * time.Millisecond
	shortWriteBound      = 3 * shortPlatformTimeout
	callGiveUp           = 5 * time.Second
)

// hangingManager accepts every request and never answers it; it records the
// method and path of each. The handler ends when the client gives up.
func hangingManager(t *testing.T, answer func(w http.ResponseWriter, r *http.Request) bool) (*httptest.Server, chan string) {
	t.Helper()
	release := make(chan struct{})
	requests := make(chan string, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- r.Method + " " + r.URL.Path:
		default:
		}
		if answer != nil && answer(w, r) {
			return
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

// within runs call and fails the test when it has not returned after callGiveUp.
func within[T any](t *testing.T, call func() T) (T, time.Duration) {
	t.Helper()
	done := make(chan T, 1)
	started := time.Now()
	go func() { done <- call() }()
	select {
	case result := <-done:
		return result, time.Since(started)
	case <-time.After(callGiveUp):
		t.Fatalf("the call is still waiting for the device-manager after %v", callGiveUp)
		var zero T
		return zero, 0
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Against a device-manager that never answers, a read ends at the read timeout
// and a write at its own, longer bound.
func TestADeviceManagerThatNeverAnswersEndsAtTheBoundOfTheCall(t *testing.T) {
	server, _ := hangingManager(t, nil)
	repo := &StateRepo{externalWriteTimeout: shortWriteBound}
	repo.Config.DeviceManagerUrl = server.URL
	repo.Config.PlatformHttpTimeout = shortPlatformTimeout

	bounds := map[string]time.Duration{"device type": shortPlatformTimeout, "device types": shortPlatformTimeout, "create": shortWriteBound, "delete": shortWriteBound}
	calls := map[string]func() error{
		"device type": func() error {
			_, err := repo.GetIotDeviceType(context.Background(), crudToken, "dt")
			return err
		},
		"device types": func() error {
			_, err := repo.GetIotDeviceTypes(context.Background(), crudToken)
			return err
		},
		"create": func() error {
			_, err := repo.GenerateExternalDevice(context.Background(), crudToken, CreateDeviceByTypeRequest{DeviceTypeId: "dt", Name: "n"})
			return err
		},
		"delete": func() error {
			return repo.DeleteExternalDevice(context.Background(), crudToken, "external-d")
		},
	}
	for name, call := range calls {
		err, elapsed := within(t, call)
		if !isTimeout(err) {
			t.Errorf("%s: expected a timeout error, got %v", name, err)
		}
		if elapsed < bounds[name] {
			t.Errorf("%s: gave up after %v, before its bound of %v", name, elapsed, bounds[name])
		}
		if bounds[name] == shortPlatformTimeout && elapsed >= shortWriteBound {
			t.Errorf("%s: a read took %v, past the read timeout of %v", name, elapsed, shortPlatformTimeout)
		}
	}
}

// The shared read client lib.New sets wins over the configured timeout.
func TestTheSharedPlatformClientIsUsedWhenSet(t *testing.T) {
	server, _ := hangingManager(t, nil)
	repo := &StateRepo{PlatformClients: platformhttp.Clients{Read: &http.Client{Timeout: shortPlatformTimeout}}}
	repo.Config.DeviceManagerUrl = server.URL
	repo.Config.PlatformHttpTimeout = time.Hour

	err, _ := within(t, func() error {
		_, err := repo.GetIotDeviceType(context.Background(), crudToken, "dt")
		return err
	})
	if !isTimeout(err) {
		t.Fatalf("expected the timeout of the shared client, got %v", err)
	}
}

// A read gives up with the caller; a write is sent even for a caller that left,
// since the device-manager may apply it anyway and only the answer tells.
func TestOnlyTheReadsFollowTheCancellationOfTheCaller(t *testing.T) {
	server, requests := hangingManager(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet {
			return false
		}
		_, _ = w.Write([]byte(`{"id":"external-device"}`))
		return true
	})
	repo := &StateRepo{}
	repo.Config.DeviceManagerUrl = server.URL
	repo.Config.PlatformHttpTimeout = callGiveUp

	ctx, cancel := context.WithCancel(context.Background())
	readErr := make(chan error, 1)
	go func() {
		_, err := repo.GetIotDeviceType(ctx, crudToken, "dt")
		readErr <- err
	}()
	select {
	case <-requests:
	case <-time.After(callGiveUp):
		t.Fatal("the read never reached the device-manager")
	}
	cancel()
	select {
	case err := <-readErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected the cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the read did not give up with its caller")
	}

	device, err := repo.GenerateExternalDevice(ctx, crudToken, CreateDeviceByTypeRequest{DeviceTypeId: "dt", Name: "n"})
	if err != nil || device.Id != "external-device" {
		t.Errorf("create: expected the created device despite the cancelled caller, got %+v, %v", device, err)
	}
	if err := repo.DeleteExternalDevice(ctx, crudToken, "external-device"); err != nil {
		t.Errorf("delete: expected the delete despite the cancelled caller, got %v", err)
	}
}

type createResult struct {
	result DeviceResponse
	access bool
	exists bool
	err    error
}

// A hanging device-manager fails CreateDeviceByType like any other device-manager
// error, whichever of its two calls hangs, and the world stays as it was: the read
// at the read timeout, the write at its bound.
func TestCreateDeviceByTypeFailsOnAHangingDeviceManager(t *testing.T) {
	for _, hanging := range []string{http.MethodGet, http.MethodPost} {
		t.Run(hanging, func(t *testing.T) {
			server, _ := hangingManager(t, func(w http.ResponseWriter, r *http.Request) bool {
				switch {
				case r.Method == hanging:
					return false
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/device-types/"):
					_, _ = w.Write([]byte(`{"id":"dt","services":[{"id":"external-service","name":"svc"}]}`))
				default:
					_, _ = w.Write([]byte(`{"id":"external-device","device_type_id":"dt"}`))
				}
				return true
			})
			repo, store := pathTestRepo(t)
			repo.Config.DeviceManagerUrl = server.URL
			repo.Config.PlatformHttpTimeout = shortPlatformTimeout
			repo.externalWriteTimeout = shortWriteBound
			before, _ := store.stored(t, "w")

			created, elapsed := within(t, func() createResult {
				result, access, exists, err := repo.CreateDeviceByType(context.Background(), crudToken, CreateDeviceByTypeRequest{DeviceTypeId: "dt", Room: "r", Name: "typed"})
				return createResult{result, access, exists, err}
			})
			if !isTimeout(created.err) {
				t.Fatalf("expected a timeout error, got %v", created.err)
			}
			if hanging == http.MethodPost && elapsed < shortWriteBound {
				t.Errorf("the write gave up after %v, before its bound of %v", elapsed, shortWriteBound)
			}
			expectFlags(t, created.access, created.exists, true, true)
			after, _ := store.stored(t, "w")
			if len(after.Rooms["r"].Devices) != len(before.Rooms["r"].Devices) {
				t.Errorf("expected no device to be added, the room went from %d to %d devices", len(before.Rooms["r"].Devices), len(after.Rooms["r"].Devices))
			}
		})
	}
}

// DeleteDevice removes the device from the world and only logs a failed platform
// delete; a hanging one is such a failure and must not hold the call.
func TestDeleteDeviceSucceedsOnAHangingDeviceManager(t *testing.T) {
	server, requests := hangingManager(t, nil)
	repo, store := pathTestRepo(t)
	repo.Config.DeviceManagerUrl = server.URL
	repo.Config.PlatformHttpTimeout = shortPlatformTimeout
	repo.externalWriteTimeout = shortWriteBound

	deleted, _ := within(t, func() createResult {
		result, access, exists, err := repo.DeleteDevice(context.Background(), crudToken, "d")
		return createResult{result, access, exists, err}
	})
	if deleted.err != nil {
		t.Fatalf("expected the failed platform delete to be logged only, got %v", deleted.err)
	}
	expectFlags(t, deleted.access, deleted.exists, true, true)
	select {
	case request := <-requests:
		if request != "DELETE /devices/external-d" {
			t.Errorf("expected the platform device to be deleted, got %q", request)
		}
	default:
		t.Error("the device-manager was never asked to delete the platform device")
	}
	world, _ := store.stored(t, "w")
	if world.Rooms["r"].Devices["d"] != nil {
		t.Error("the device is still in the stored world")
	}
}

// A device-manager write slower than the read timeout still completes: the
// device may already exist on the platform, and only the answer puts it into the world.
func TestCreateDeviceByTypeWaitsForASlowWrite(t *testing.T) {
	server, _ := hangingManager(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"id":"dt","services":[{"id":"external-service","name":"svc"}]}`))
			return true
		}
		time.Sleep(2 * shortPlatformTimeout)
		_, _ = w.Write([]byte(`{"id":"external-device","device_type_id":"dt"}`))
		return true
	})
	repo, store := pathTestRepo(t)
	repo.Config.DeviceManagerUrl = server.URL
	repo.Config.PlatformHttpTimeout = shortPlatformTimeout

	created, _ := within(t, func() createResult {
		result, access, exists, err := repo.CreateDeviceByType(context.Background(), crudToken, CreateDeviceByTypeRequest{DeviceTypeId: "dt", Room: "r", Name: "typed"})
		return createResult{result, access, exists, err}
	})
	if created.err != nil {
		t.Fatalf("expected the slow write to complete, got %v", created.err)
	}
	world, _ := store.stored(t, "w")
	if device := world.Rooms["r"].Devices[created.result.Device.Id]; device == nil || device.ExternalRef != "external-device" {
		t.Error("the created device is not in the stored world")
	}
}
