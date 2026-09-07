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
	"context"
	"encoding/json"
	"errors"
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
	points, err := New(server.URL).Fetch(context.Background(), "Bearer abc", "device-1", "service-1", "energy.value", start, end)
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

	points, err := New(server.URL).Fetch(context.Background(), "t", "d", "s", "value", time.Now().Add(-time.Hour), time.Now())
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

// A fetch belongs to the load that asked for it. When that load is over -
// cancelled, or out of time - the request has to end with it rather than run on
// under the client's own timeout, which is minutes long.
func TestFetchEndsWithTheContextItWasGiven(t *testing.T) {
	server := wrapperAnswering(t, `[[["2026-01-05T00:00:00Z", 1.5],["2026-01-05T00:15:00Z", 2.5]]]`, nil)
	defer server.Close()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	//a deadline in the past rather than a short one: no clock has to elapse for
	//this to be decided, so the test cannot be slow and cannot be flaky
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"cancelled", cancelled, context.Canceled},
		{"deadline in the past", expired, context.DeadlineExceeded},
	} {
		_, err := New(server.URL).Fetch(tc.ctx, "t", "d", "s", "value", time.Now().Add(-time.Hour), time.Now())
		if err == nil {
			t.Errorf("%s: a spent context has to end the fetch", tc.name)
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: expected %v, got %v", tc.name, tc.want, err)
		}
	}
}

// The same for a wrapper that accepts the request and then says nothing: the
// caller's deadline, not the client's, decides when to give up.
func TestFetchGivesUpWhenTheCallersDeadlinePasses(t *testing.T) {
	released := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-released
	}))
	//released first, then Close: httptest.Server.Close waits for the handlers
	//still in flight, and this one only ends when it is let go
	defer func() {
		close(released)
		server.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := New(server.URL).Fetch(ctx, "t", "d", "s", "value", time.Now().Add(-time.Hour), time.Now())
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("expected the caller's deadline to end the fetch, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the fetch outlived the context it was given")
	}
}

func TestFetchRefusals(t *testing.T) {
	for _, tc := range []struct{ name, body, fragment string }{
		{"empty window", `[[]]`, "at least 2"},
		{"the wrapper's padded empty result", `[[[null, null]]]`, "at least 2"},
		{"only gaps", `[[["2026-01-05T00:00:00Z", null],["2026-01-05T00:15:00Z", null]]]`, "at least 2"},
		{"wrong shape", `{"nope": 1}`, "unreadable wrapper response"},
		{"two series", `[[],[]]`, "expected one series"},
		//only the all-null row is the wrapper's padding; a value under no
		//instant is an answer this client cannot place in time
		{"a value without an instant", `[[[null, 1.5],["2026-01-05T00:00:00Z", 1.0],["2026-01-05T00:15:00Z", 2.0]]]`, "unreadable timestamp"},
	} {
		server := wrapperAnswering(t, tc.body, nil)
		_, err := New(server.URL).Fetch(context.Background(), "t", "d", "s", "value", time.Now().Add(-time.Hour), time.Now())
		server.Close()
		if err == nil || !strings.Contains(err.Error(), tc.fragment) {
			t.Errorf("%s: expected %q, got %v", tc.name, tc.fragment, err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no access", http.StatusNotFound)
	}))
	defer server.Close()
	_, err := New(server.URL).Fetch(context.Background(), "t", "d", "s", "value", time.Now().Add(-time.Hour), time.Now())
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a non-200 has to carry the status, got %v", err)
	}
}

// TestHasReadingsPinsTheRequestContract: the answer decides whether a history
// run is refused, so the request has to ask for the first row of the window and
// nothing else - the limit alone would leave the row the wrapper picks open.
func TestHasReadingsPinsTheRequestContract(t *testing.T) {
	captured := map[string]interface{}{}
	server := wrapperAnswering(t, `[[["2026-01-01T00:00:00Z", 1.5]]]`, &captured)
	defer server.Close()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	occupied, err := New(server.URL).HasReadings(context.Background(), "Bearer abc", "device-1", "service-1", "energy.value", start, end)
	if err != nil {
		t.Fatal(err)
	}
	if !occupied {
		t.Error("a row in the window means the window is occupied")
	}
	if captured["path"] != "/queries" || captured["auth"] != "Bearer abc" {
		t.Errorf("path/auth: %v %v", captured["path"], captured["auth"])
	}
	element := captured["element"].(map[string]interface{})
	if element["deviceId"] != "device-1" || element["serviceId"] != "service-1" {
		t.Errorf("ids: %v", element)
	}
	if limit, _ := element["limit"].(float64); limit != 1 {
		t.Errorf("expected limit 1, got %v", element["limit"])
	}
	if index, ok := element["orderColumnIndex"].(float64); !ok || index != 0 {
		t.Errorf("the order column index has to be sent as 0, got %v", element["orderColumnIndex"])
	}
	if element["orderDirection"] != "asc" {
		t.Errorf("expected an ascending order, got %v", element["orderDirection"])
	}
	//the wrapper compares with time > start and time < end, so the start is a
	//millisecond before the window and a reading exactly at it is still seen
	timeElement := element["time"].(map[string]interface{})
	if timeElement["start"] != "2025-12-31T23:59:59.999Z" || timeElement["end"] != "2026-01-02T00:00:00Z" {
		t.Errorf("the window has to be the one that was asked for, a millisecond wider at the start, got %v", timeElement)
	}
	column := element["columns"].([]interface{})[0].(map[string]interface{})
	if column["name"] != "energy.value" {
		t.Errorf("column: %v", column)
	}
}

// TestHasReadingsKeepsTheSubSecondPartOfTheWindow: the window is truncated to
// whole milliseconds upstream, and RFC3339 would drop exactly those digits - a
// start rendered a second early can see a reading that lies before the window.
func TestHasReadingsKeepsTheSubSecondPartOfTheWindow(t *testing.T) {
	captured := map[string]interface{}{}
	server := wrapperAnswering(t, `[[[null, null]]]`, &captured)
	defer server.Close()

	start := time.Date(2026, 7, 1, 12, 0, 0, 250*int(time.Millisecond), time.UTC)
	if _, err := New(server.URL).HasReadings(context.Background(), "t", "d", "s", "value",
		start, start.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	timeElement := captured["element"].(map[string]interface{})["time"].(map[string]interface{})
	if timeElement["start"] != "2026-07-01T12:00:00.249Z" || timeElement["end"] != "2026-07-02T12:00:00.25Z" {
		t.Errorf("the milliseconds of the window were lost: %v", timeElement)
	}
}

// TestHasReadingsReadsBothAnswers, the null row included: with a limit of one
// nothing behind that row is visible, so it counts as a reading rather than as
// an empty window.
func TestHasReadingsReadsBothAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows string
		want bool
	}{
		{"a row with a value", `[[["2026-01-01T00:00:00Z", 1.5]]]`, true},
		{"a row with a zero value", `[[["2026-01-01T00:00:00Z", 0]]]`, true},
		{"a row whose value is null", `[[["2026-07-01T00:00:00Z", null]]]`, true},
		//what an empty window really answers: the wrapper pads the result with
		//an all-null row rather than sending none, and the per_query trim is
		//undone by the re-slice to the limit
		{"the wrapper's padded empty result", `[[[null, null]]]`, false},
		{"a padded row that carries a value but no instant", `[[[null, 1.5]]]`, false},
		{"no rows at all", `[[]]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := wrapperAnswering(t, tc.rows, nil)
			defer server.Close()
			occupied, err := New(server.URL).HasReadings(context.Background(), "t", "d", "s", "value",
				time.Now().Add(-24*time.Hour), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if occupied != tc.want {
				t.Errorf("expected %v, got %v", tc.want, occupied)
			}
		})
	}
}

// TestHasReadingsRefusesAnAnswerItCannotRead: the caller starts no run on an
// error, so an answer this client cannot interpret has to be one rather than a
// window reported as free.
func TestHasReadingsRefusesAnAnswerItCannotRead(t *testing.T) {
	for _, tc := range []struct{ name, body, fragment string }{
		{"an instant that is a number", `[[[1751328000, 1.5]]]`, "unreadable timestamp"},
		{"an instant that is not rfc3339", `[[["yesterday", 1.5]]]`, "unreadable timestamp"},
		{"wrong shape", `{"nope": 1}`, "unreadable wrapper response"},
		{"two series", `[[],[]]`, "expected one series"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := wrapperAnswering(t, tc.body, nil)
			defer server.Close()
			occupied, err := New(server.URL).HasReadings(context.Background(), "t", "d", "s", "value",
				time.Now().Add(-24*time.Hour), time.Now())
			if err == nil || !strings.Contains(err.Error(), tc.fragment) {
				t.Errorf("expected %q, got %v", tc.fragment, err)
			}
			if occupied {
				t.Error("an error has to answer false, so nothing reads the answer as occupied")
			}
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no access", http.StatusForbidden)
	}))
	defer server.Close()
	_, err := New(server.URL).HasReadings(context.Background(), "t", "d", "s", "value",
		time.Now().Add(-24*time.Hour), time.Now())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("a non-200 has to carry the status, got %v", err)
	}
	//and it carries the status as a number, which is what tells a caller a
	//platform state from an outage
	status := &StatusError{}
	if !errors.As(err, &status) || status.Status != http.StatusForbidden {
		t.Errorf("expected a *StatusError carrying 403, got %#v", err)
	}
}

// TestHasReadingsEndsWithTheContextItWasGiven: the check runs while the caller
// waits on a POST, so its budget has to end the request - and it is the client
// that reports it, which is why the caller of the check does not look at the
// context itself before it asks.
func TestHasReadingsEndsWithTheContextItWasGiven(t *testing.T) {
	server := wrapperAnswering(t, `[[["2026-01-01T00:00:00Z", 1.5]]]`, nil)
	defer server.Close()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	//a deadline in the past rather than a short one: nothing has to elapse for
	//this to be decided, so the test is neither slow nor flaky
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"cancelled", cancelled, context.Canceled},
		{"deadline in the past", expired, context.DeadlineExceeded},
	} {
		occupied, err := New(server.URL).HasReadings(tc.ctx, "t", "d", "s", "value",
			time.Now().Add(-24*time.Hour), time.Now())
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: expected %v, got %v", tc.name, tc.want, err)
		}
		if occupied {
			t.Errorf("%s: a spent context must not answer that the window holds readings", tc.name)
		}
	}
}

// TestFetchSendsNoOrder keeps the request Fetch makes as it was: the wrapper
// returns the whole window to it, and an order it did not ask for before is a
// change to a query that works.
func TestFetchSendsNoOrder(t *testing.T) {
	captured := map[string]interface{}{}
	server := wrapperAnswering(t, `[[["2026-01-05T00:00:00Z", 1.5],["2026-01-05T00:15:00Z", 2.5]]]`, &captured)
	defer server.Close()

	if _, err := New(server.URL).Fetch(context.Background(), "t", "d", "s", "value",
		time.Now().Add(-time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	element := captured["element"].(map[string]interface{})
	if _, sent := element["orderColumnIndex"]; sent {
		t.Errorf("Fetch sent an order column index: %v", element)
	}
	if _, sent := element["orderDirection"]; sent {
		t.Errorf("Fetch sent an order direction: %v", element)
	}
}
