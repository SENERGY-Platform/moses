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

package timeseries

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func wrapperAnswering(t *testing.T, rows string, capture *map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			body := []map[string]interface{}{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			(*capture)["path"] = r.URL.Path
			(*capture)["query"] = r.URL.RawQuery
			(*capture)["auth"] = r.Header.Get("Authorization")
			if len(body) > 0 {
				(*capture)["element"] = body[0]
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rows))
	}))
}

func TestFetchPinsTheRequestContract(t *testing.T) {
	captured := map[string]interface{}{}
	server := wrapperAnswering(t, `[[["2026-01-05T00:00:00Z", 1.5],["2026-01-05T00:15:00Z", 2.5]]]`, &captured)
	defer server.Close()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	points, err := New(server.URL).Fetch("Bearer abc", "device-1", "service-1", "energy.value", start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 || points[0].Value != 1.5 {
		t.Fatalf("unexpected points: %+v", points)
	}
	if captured["path"] != "/queries" || captured["auth"] != "Bearer abc" {
		t.Errorf("path/auth: %v %v", captured["path"], captured["auth"])
	}
	if !strings.Contains(captured["query"].(string), "format=per_query") || !strings.Contains(captured["query"].(string), "time_format=") {
		t.Errorf("the format and time_format have to be pinned, got %q", captured["query"])
	}
	element := captured["element"].(map[string]interface{})
	if element["deviceId"] != "device-1" || element["serviceId"] != "service-1" {
		t.Errorf("ids: %v", element)
	}
	timeElement := element["time"].(map[string]interface{})
	if timeElement["start"] != "2026-01-01T00:00:00Z" || timeElement["end"] != "2026-01-08T00:00:00Z" {
		t.Errorf("the window has to be concrete rfc3339 times, got %v", timeElement)
	}
	column := element["columns"].([]interface{})[0].(map[string]interface{})
	if column["name"] != "energy.value" {
		t.Errorf("column: %v", column)
	}
}

func TestFetchSortsDeduplicatesAndSkipsGaps(t *testing.T) {
	//out of order, one null gap, one duplicate timestamp
	server := wrapperAnswering(t, `[[
		["2026-01-05T00:30:00Z", 3.0],
		["2026-01-05T00:00:00Z", 1.0],
		["2026-01-05T00:15:00Z", null],
		["2026-01-05T00:30:00Z", 99.0],
		["2026-01-05T00:45:00Z", 4.0]
	]]`, nil)
	defer server.Close()

	points, err := New(server.URL).Fetch("t", "d", "s", "value", time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 3 {
		t.Fatalf("expected 3 usable points, got %+v", points)
	}
	for i := 1; i < len(points); i++ {
		if points[i].Unix <= points[i-1].Unix {
			t.Fatalf("not strictly increasing: %+v", points)
		}
	}
}

func TestFetchRefusals(t *testing.T) {
	for _, tc := range []struct{ name, body, fragment string }{
		{"empty window", `[[]]`, "at least 2"},
		{"only gaps", `[[["2026-01-05T00:00:00Z", null],["2026-01-05T00:15:00Z", null]]]`, "at least 2"},
		{"wrong shape", `{"nope": 1}`, "unreadable wrapper response"},
		{"two series", `[[],[]]`, "expected one series"},
	} {
		server := wrapperAnswering(t, tc.body, nil)
		_, err := New(server.URL).Fetch("t", "d", "s", "value", time.Now().Add(-time.Hour), time.Now())
		server.Close()
		if err == nil || !strings.Contains(err.Error(), tc.fragment) {
			t.Errorf("%s: expected %q, got %v", tc.name, tc.fragment, err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no access", http.StatusNotFound)
	}))
	defer server.Close()
	_, err := New(server.URL).Fetch("t", "d", "s", "value", time.Now().Add(-time.Hour), time.Now())
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a non-200 has to carry the status, got %v", err)
	}
}
