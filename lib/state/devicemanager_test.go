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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sc_jwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
)

// These tests are the ones that used to live on lib/jwt's JwtImpersonate. The
// helpers replaced it, the behaviour did not change, so the assertions moved
// here rather than being deleted. Everything runs against a loopback server; no
// container is needed.

func TestTheCallersTokenIsForwardedVerbatim(t *testing.T) {
	// including the scheme: the device-manager gets the header the caller sent,
	// which is what makes it apply the caller's permissions
	seen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen <- request.Header.Get("Authorization")
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()

	result := map[string]interface{}{}
	if err := deviceManagerGetJson("Bearer head.payload.sig", server.URL, &result); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if forwarded := <-seen; forwarded != "Bearer head.payload.sig" {
		t.Errorf("expected the token to be forwarded verbatim, got %q", forwarded)
	}
}

func TestGetJsonDecodesTheResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"id":"a","count":2}`))
	}))
	defer server.Close()

	result := struct {
		Id    string `json:"id"`
		Count int    `json:"count"`
	}{}
	if err := deviceManagerGetJson("Bearer t", server.URL, &result); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.Id != "a" || result.Count != 2 {
		t.Errorf("expected {a 2}, got %+v", result)
	}
}

func TestGetJsonReturnsAnErrorForANonJsonBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("<html>not json</html>"))
	}))
	defer server.Close()

	result := map[string]interface{}{}
	if err := deviceManagerGetJson("Bearer t", server.URL, &result); err == nil {
		t.Error("expected a decoding error, got nil")
	}
}

func TestPostJsonSendsTheBodyAsJsonAndDecodesTheResponse(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	seenBody := make(chan string, 1)
	seenContentType := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		seenBody <- strings.TrimSpace(string(body))
		seenContentType <- request.Header.Get("Content-Type")
		_, _ = writer.Write([]byte(`{"name":"echo"}`))
	}))
	defer server.Close()

	result := payload{}
	if err := deviceManagerPostJson("Bearer t", server.URL, payload{Name: "request"}, &result); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if body := <-seenBody; body != `{"name":"request"}` {
		t.Errorf("expected the json encoded request body, got %q", body)
	}
	if contentType := <-seenContentType; contentType != "application/json" {
		t.Errorf("expected content type application/json, got %q", contentType)
	}
	if result.Name != "echo" {
		t.Errorf("expected the decoded response {echo}, got %+v", result)
	}
}

func TestPostJsonSkipsDecodingWhenNoResultIsWanted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`this is not json`))
	}))
	defer server.Close()

	if err := deviceManagerPostJson("Bearer t", server.URL, map[string]string{}, nil); err != nil {
		t.Fatalf("expected a nil result to skip decoding, got %v", err)
	}
}

func TestPostJsonReturnsTheEncodingErrorBeforeSendingAnything(t *testing.T) {
	requests := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests <- struct{}{}
	}))
	defer server.Close()

	// channels cannot be json encoded
	if err := deviceManagerPostJson("Bearer t", server.URL, make(chan int), nil); err == nil {
		t.Fatal("expected a json encoding error, got nil")
	}
	select {
	case <-requests:
		t.Error("expected no request to be sent when encoding fails")
	default:
	}
}

func TestDeleteSucceedsOnAPlain200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodDelete {
			t.Errorf("expected a DELETE, got %s", request.Method)
		}
		_, _ = writer.Write([]byte("ok"))
	}))
	defer server.Close()

	if err := deviceManagerDelete("Bearer t", server.URL); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestA401BecomesAccessDenied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte("nope"))
	}))
	defer server.Close()

	calls := map[string]func(string) error{
		"get":    func(url string) error { return deviceManagerGetJson("Bearer t", url, &map[string]interface{}{}) },
		"post":   func(url string) error { return deviceManagerPostJson("Bearer t", url, map[string]string{}, nil) },
		"delete": func(url string) error { return deviceManagerDelete("Bearer t", url) },
	}
	for name, call := range calls {
		err := call(server.URL)
		if !errors.Is(err, errAccessDenied) {
			t.Errorf("%s: expected access denied, got %v", name, err)
		}
		if err != nil && err.Error() != "access denied" {
			t.Errorf("%s: expected the message \"access denied\", got %q", name, err.Error())
		}
	}
}

// CURRENT BEHAVIOUR, DELIBERATELY KEPT: the check is "not 200", so every other
// 2xx counts as a failure. A device-manager answering 201 Created to a POST or
// 204 No Content to a DELETE looks like an error here even though it succeeded,
// and the caller cannot tell that apart from a 500. This came over unchanged
// from lib/jwt; the test is here so the question stays visible instead of being
// rediscovered the next time a downstream service changes its status codes.
func TestEverySuccessStatusOtherThan200IsTreatedAsAnError(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusAccepted, http.StatusNoContent, http.StatusInternalServerError} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.WriteHeader(status)
		}))

		err := deviceManagerGetJson("Bearer t", server.URL, &map[string]interface{}{})
		if err == nil {
			t.Errorf("status %d: expected an error, got nil", status)
		} else if err.Error() != "unexpected statuscode in response for GET "+server.URL {
			t.Errorf("status %d: unexpected message %q", status, err.Error())
		}

		err = deviceManagerDelete("Bearer t", server.URL)
		if err == nil {
			t.Errorf("status %d: expected an error from delete, got nil", status)
		} else if err.Error() != "unexpected statuscode in response for DELETE "+server.URL {
			t.Errorf("status %d: unexpected message %q", status, err.Error())
		}

		err = deviceManagerPostJson("Bearer t", server.URL, map[string]string{}, nil)
		if err == nil {
			t.Errorf("status %d: expected an error from post, got nil", status)
		} else if err.Error() != "unexpected statuscode in response for POST "+server.URL {
			t.Errorf("status %d: unexpected message %q", status, err.Error())
		}

		server.Close()
	}
}

func TestAnUnusableUrlIsAnErrorAndNeverAPanic(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("panicked on an unusable url: %v", recovered)
		}
	}()

	if err := deviceManagerGetJson("Bearer t", "http://%zz", &struct{}{}); err == nil {
		t.Error("get: expected an error for an unparsable url, got nil")
	}
	if err := deviceManagerPostJson("Bearer t", "http://%zz", map[string]string{}, nil); err == nil {
		t.Error("post: expected an error for an unparsable url, got nil")
	}
	if err := deviceManagerDelete("Bearer t", "http://%zz"); err == nil {
		t.Error("delete: expected an error for an unparsable url, got nil")
	}
}

// The device type id goes into the path, and ids are urns with colons in them.
// An id that tries to leave its path segment must not turn into a request
// against another endpoint.
func TestTheDeviceTypeIdIsEscapedIntoThePath(t *testing.T) {
	seen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen <- request.URL.EscapedPath()
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()

	repo := &StateRepo{}
	repo.Config.DeviceManagerUrl = server.URL
	if _, err := repo.GetIotDeviceType(sc_jwt.Token{Token: "Bearer t", Sub: "user"}, "urn:infai:ses:device-type:a/../b"); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	path := <-seen
	if path != "/device-types/urn:infai:ses:device-type:a%2F..%2Fb" {
		t.Errorf("expected the id to stay inside its path segment, got %q", path)
	}
}
