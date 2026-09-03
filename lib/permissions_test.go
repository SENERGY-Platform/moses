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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	permModel "github.com/SENERGY-Platform/permissions-v2/pkg/model"
)

type recordedRequest struct {
	method        string
	path          string
	rawQuery      string
	authorization string
	body          string
}

// permissionsServer stands in for permissions-v2 and records what arrived.
func permissionsServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*permissionsClient, *[]recordedRequest) {
	t.Helper()
	recorded := &[]recordedRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		//put it back, the handler under test may want to echo it
		r.Body = io.NopCloser(bytes.NewReader(raw))
		*recorded = append(*recorded, recordedRequest{
			//EscapedPath, not Path: the point is what went over the wire
			method: r.Method, path: r.URL.EscapedPath(), rawQuery: r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"), body: string(raw),
		})
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return newPermissionsClient(server.URL), recorded
}

func TestReadingTheRightsOfADeviceAddressesTheManageEndpoint(t *testing.T) {
	client, recorded := permissionsServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(permModel.Resource{
			Id: "urn:infai:ses:device:a/b", TopicId: "devices",
			ResourcePermissions: permModel.ResourcePermissions{
				UserPermissions: map[string]permModel.PermissionsMap{"user-a": {Administrate: true}},
			},
		})
	})

	resource, err, code := client.GetResource(context.Background(), "Bearer token-of-the-caller", "devices", "urn:infai:ses:device:a/b")
	if err != nil || code != http.StatusOK {
		t.Fatalf("expected a read, got %v (%d)", err, code)
	}
	if !resource.UserPermissions["user-a"].Administrate {
		t.Errorf("the answer has to be decoded, got %+v", resource)
	}
	if len(*recorded) != 1 {
		t.Fatalf("expected one request, got %d", len(*recorded))
	}
	request := (*recorded)[0]
	//the id is escaped, or a device id with a slash in it would address another
	//endpoint entirely
	if request.method != http.MethodGet || request.path != "/manage/devices/urn:infai:ses:device:a%2Fb" {
		t.Errorf("unexpected request: %v %v", request.method, request.path)
	}
	if request.authorization != "Bearer token-of-the-caller" {
		t.Errorf("the caller's token has to be forwarded verbatim, got %q", request.authorization)
	}
	//the version permissions-v2 checks, so a breaking version answers 426
	//instead of being talked to in the wrong protocol
	if !strings.Contains(request.rawQuery, "version="+permModel.ClientVersion) {
		t.Errorf("expected the client version in the query, got %q", request.rawQuery)
	}
}

func TestWritingTheRightsOfADeviceSendsThemAsJson(t *testing.T) {
	client, recorded := permissionsServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_, _ = w.Write(raw)
	})

	rights := permModel.ResourcePermissions{
		UserPermissions: map[string]permModel.PermissionsMap{
			"user-a":    {Read: true, Write: true, Execute: true, Administrate: true},
			"demo-user": {Read: true, Execute: true},
		},
	}
	result, err, code := client.SetPermission(context.Background(), "Bearer token", "devices", "dev-1", rights)
	if err != nil || code != http.StatusOK {
		t.Fatalf("expected a write, got %v (%d)", err, code)
	}
	if !result.UserPermissions["demo-user"].Read || result.UserPermissions["demo-user"].Administrate {
		t.Errorf("the answer has to be decoded, got %+v", result)
	}
	request := (*recorded)[0]
	if request.method != http.MethodPut || request.path != "/manage/devices/dev-1" {
		t.Errorf("unexpected request: %v %v", request.method, request.path)
	}
	if !strings.Contains(request.body, `"execute":true`) {
		t.Errorf("the whole rights object has to be sent, got %s", request.body)
	}
}

// The reason permissions-v2 gives is what a caller needs: a group they may not
// share with is a 400 with an explanation, and it travels into the 502 of the
// share endpoint.
func TestARefusalCarriesTheStatusAndTheReasonAndNotTheToken(t *testing.T) {
	client, _ := permissionsServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "user may not share with group /strangers", http.StatusBadRequest)
	})

	_, err, code := client.SetPermission(context.Background(), "Bearer super-secret-token", "devices", "dev-1",
		permModel.ResourcePermissions{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if code != http.StatusBadRequest {
		t.Errorf("expected the status to be reported, got %d", code)
	}
	if !strings.Contains(err.Error(), "/strangers") || !strings.Contains(err.Error(), "400") {
		t.Errorf("expected the reason and the status in the message, got %v", err)
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("the token must never reach an error message, got %v", err)
	}
}

// The generated client would hang here: it has no timeout and drops the context.
func TestACallThatIsNotAnsweredEndsOnTheClientTimeout(t *testing.T) {
	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
	}))
	//LIFO: the handler has to be released before Close waits for it
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(blocked) })
	client := &permissionsClient{url: server.URL, client: &http.Client{Timeout: 50 * time.Millisecond}}

	started := time.Now()
	_, err, _ := client.GetResource(context.Background(), "Bearer token", "devices", "dev-1")
	if err == nil {
		t.Fatal("expected the call to end")
	}
	if time.Since(started) > 2*time.Second {
		t.Errorf("expected the client timeout to end it, took %v", time.Since(started))
	}
}

// A caller that went away must not leave the calls running.
func TestACancelledContextEndsTheCall(t *testing.T) {
	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
	}))
	//LIFO: the handler has to be released before Close waits for it
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(blocked) })
	client := newPermissionsClient(server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	started := time.Now()
	_, err, _ := client.GetResource(ctx, "Bearer token", "devices", "dev-1")
	if err == nil {
		t.Fatal("expected the call to end")
	}
	if time.Since(started) > 2*time.Second {
		t.Errorf("expected the context to end it, took %v", time.Since(started))
	}
}

// A trailing slash in the configured url must not produce a double slash in the
// path, which some gateways answer with a redirect.
func TestATrailingSlashInTheUrlIsTrimmed(t *testing.T) {
	client := newPermissionsClient("http://permv2.permissions:8080/")
	if got := client.resourceUrl("devices", "dev-1"); !strings.HasPrefix(got, "http://permv2.permissions:8080/manage/devices/dev-1?") {
		t.Errorf("unexpected url: %v", got)
	}
}
