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
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/scripthttp"
)

// loopbackScriptHTTP is the production client plus the loopback interface, so a
// script reaches the httptest servers of a test.
func loopbackScriptHTTP(t *testing.T) *scripthttp.Client {
	t.Helper()
	client, err := scripthttp.New("", scripthttp.WithAddressCheck(func(addr netip.Addr) bool {
		return addr.Unmap().IsLoopback() || scripthttp.IsPublic(addr)
	}))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// A legacy script with the production client, or with none, gets an empty
// string for a server on 127.0.0.1, which is never reached.
func TestLegacyHttpGetCannotReachLoopback(t *testing.T) {
	hits := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("reached"))
	}))
	t.Cleanup(server.Close)
	strict, err := scripthttp.New("")
	if err != nil {
		t.Fatal(err)
	}
	for name, client := range map[string]*scripthttp.Client{"production client": strict, "no client": nil} {
		got := []string{}
		moses := map[string]interface{}{"got": func(value string) { got = append(got, value) }}
		if err := run(fmt.Sprintf(`moses.got(httpGet(%q));`, server.URL), moses, time.Second, nil, nil, "w", "c", client); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 1 || got[0] != "" {
			t.Fatalf("%s: expected an empty string, got %q", name, got)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the loopback server was reached %d times", hits.Load())
	}
}
