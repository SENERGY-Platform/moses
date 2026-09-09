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

// Package timeseries fetches real platform measurements from the
// timescale-wrapper, so a dataset channel can replay them.
package timeseries

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/SENERGY-Platform/moses/lib/dataset"
)

// requestTimeout allows for the largest replay window, up to a year at
// minute resolution. That fetch runs once, when the environment starts, not
// on the publish tick, so a generous timeout does not cost anything while
// the environment is running.
//
// Applied per request on a context derived from the caller's, rather than as
// http.Client.Timeout, so the caller's own budget can still end the call
// earlier - nested deadlines take the nearer one by themselves.
const requestTimeout = 180 * time.Second

// maxPoints bounds one fetch the way dataset.MaxRows bounds one upload.
const maxPoints = dataset.MaxRows

type Client struct {
	BaseUrl string
	client  *http.Client
}

func New(baseUrl string) *Client {
	//no http.Client.Timeout: the bound lives on the request context, so that a
	//caller cancelling its load stops the fetch instead of waiting it out
	return &Client{BaseUrl: baseUrl, client: &http.Client{}}
}

// StatusError is an answer of the wrapper that was not 200. The status is part
// of the error because a caller has to tell a platform state from an outage: a
// 404 is a device this token cannot read and a 400 a column or a table the
// wrapper rejects, while a 5xx is the wrapper itself being unavailable.
type StatusError struct {
	Status  int
	Message string
}

func (this *StatusError) Error() string {
	return fmt.Sprintf("the timescale-wrapper answered %d: %s", this.Status, this.Message)
}

type queryTime struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type queryColumn struct {
	Name string `json:"name"`
}

// Series addresses one timeseries the wrapper can answer: a device's service,
// or an analytics-serving export - never both, so exactly one constructor
// fills the fields a request needs.
type Series struct {
	DeviceId  string
	ServiceId string
	ExportId  string
}

// DeviceSeries addresses a device's service.
func DeviceSeries(deviceId string, serviceId string) Series {
	return Series{DeviceId: deviceId, ServiceId: serviceId}
}

// ExportSeries addresses an analytics-serving export.
func ExportSeries(exportId string) Series {
	return Series{ExportId: exportId}
}

type queryElement struct {
	DeviceId  string        `json:"deviceId,omitempty"`
	ServiceId string        `json:"serviceId,omitempty"`
	ExportId  string        `json:"exportId,omitempty"`
	Columns   []queryColumn `json:"columns"`
	Time      queryTime     `json:"time"`
	Limit     int           `json:"limit"`

	// OrderColumnIndex and OrderDirection are sent only where a limit smaller
	// than the window needs to hit a defined row: the wrapper drops the ORDER BY
	// when the index is absent, and index 0 is the time column of a per_query
	// row. Omitted otherwise, so the request Fetch sends stays as it was.
	OrderColumnIndex *int   `json:"orderColumnIndex,omitempty"`
	OrderDirection   string `json:"orderDirection,omitempty"`
}

// queryRows posts one query element and returns the rows of the single series
// the wrapper answers with. Every request of this client goes through here, so
// the url, the headers, the error shape and the per_query response contract are
// stated once.
//
// ctx ends the call from the caller's side - a cancelled load stops the request
// and the reading of its body - and requestTimeout is nested inside it as this
// client's own upper bound, so whichever runs out first ends the request.
func (this *Client) queryRows(ctx context.Context, token string, element queryElement) ([][2]interface{}, error) {
	body, err := json.Marshal([]queryElement{element})
	if err != nil {
		return nil, err
	}
	//cancelled only when this returns: the deadline has to cover reading and
	//decoding the body too, not just the round trip to the first byte
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, this.BaseUrl+"/queries?format=per_query&time_format="+time.RFC3339, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", token)
	request.Header.Set("Content-Type", "application/json")

	response, err := this.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		//bounded: the body is not ours and an error page can be large
		message, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return nil, &StatusError{Status: response.StatusCode, Message: string(message)}
	}

	//per_query: one row array per request element, rows are [timestamp, value]
	series := [][][2]interface{}{}
	if err = json.NewDecoder(response.Body).Decode(&series); err != nil {
		return nil, fmt.Errorf("unreadable wrapper response: %w", err)
	}
	if len(series) != 1 {
		return nil, fmt.Errorf("expected one series for one request element, got %d", len(series))
	}
	return series[0], nil
}

// Fetch loads one column of one service's timeseries for [start, end) and
// refuses fewer than two usable measurements: a replay divides the elapsed
// time by the span of its series, which one point does not have. A follow
// refresh asks for the tail instead and takes any number, see FetchSince.
func (this *Client) Fetch(ctx context.Context, token string, series Series, column string, start time.Time, end time.Time) ([]dataset.Point, error) {
	points, err := this.fetchPoints(ctx, token, series, column, start, end)
	if err != nil {
		return nil, err
	}
	if len(points) < 2 {
		return nil, fmt.Errorf("the window holds %d usable measurements, replay needs at least 2", len(points))
	}
	return points, nil
}

// FetchSince is Fetch without that rule: it returns the 0..n measurements the
// window holds. A follow refresh asks for [last stored point, now], where no
// new measurement at all is the ordinary answer of a source that publishes
// less often than it is followed.
func (this *Client) FetchSince(ctx context.Context, token string, series Series, column string, start time.Time, end time.Time) ([]dataset.Point, error) {
	return this.fetchPoints(ctx, token, series, column, start, end)
}

// fetchPoints is the shared body: one request, parsed, sorted and
// deduplicated on the instant. The time_format parameter pins the wrapper's
// timestamp rendering to RFC3339, so this client does not depend on the
// wrapper's default.
func (this *Client) fetchPoints(ctx context.Context, token string, series Series, column string, start time.Time, end time.Time) ([]dataset.Point, error) {
	rows, err := this.queryRows(ctx, token, queryElement{
		DeviceId:  series.DeviceId,
		ServiceId: series.ServiceId,
		ExportId:  series.ExportId,
		Columns:   []queryColumn{{Name: column}},
		Time: queryTime{
			Start: start.UTC().Format(time.RFC3339),
			End:   end.UTC().Format(time.RFC3339),
		},
		Limit: maxPoints,
	})
	if err != nil {
		return nil, err
	}

	points := make([]dataset.Point, 0, len(rows))
	for _, row := range rows {
		if row[0] == nil {
			if row[1] == nil {
				//the wrapper pads an empty result with an all-null row rather
				//than answering no rows, so this one carries no point
				continue
			}
			//a value under no instant is not that padding row and not a point
			//either; replay needs the instant, so this is an answer this client
			//cannot read rather than a row to drop
			return nil, fmt.Errorf("unreadable timestamp: a row carries the value %v under no instant", row[1])
		}
		stamp, ok := row[0].(string)
		if !ok {
			return nil, fmt.Errorf("unreadable timestamp %v", row[0])
		}
		at, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			return nil, fmt.Errorf("unreadable timestamp %q: %w", stamp, err)
		}
		value, ok := row[1].(float64)
		if !ok {
			//a null value is a gap in the measurement, not an error
			continue
		}
		points = append(points, dataset.Point{Unix: at.Unix(), Value: value})
	}

	//replay needs strictly increasing time; the wrapper's order is its own
	sort.Slice(points, func(a, b int) bool { return points[a].Unix < points[b].Unix })
	deduplicated := points[:0]
	for i, point := range points {
		if i > 0 && point.Unix == points[i-1].Unix {
			continue
		}
		deduplicated = append(deduplicated, point)
	}
	return deduplicated, nil
}

// HasReadings reports whether one column of one service already holds a reading
// in [start, end), which is what a history run asks before it writes into a
// window. The query carries limit 1 and an explicit ascending order on the time
// column, because without the order index the wrapper drops the ORDER BY and the
// one row the limit keeps would be an arbitrary one. A row that carries an
// instant counts as a reading even where its value is null, while the all-null
// row the wrapper pads an empty result with means the window is free. A row
// that carries a value under no instant is neither: it is an answer this
// client cannot place in the window, so it is a refusal rather than a free
// window a history run would start over unchecked.
func (this *Client) HasReadings(ctx context.Context, token string, series Series, column string, start time.Time, end time.Time) (bool, error) {
	timeColumn := 0
	rows, err := this.queryRows(ctx, token, queryElement{
		DeviceId:  series.DeviceId,
		ServiceId: series.ServiceId,
		ExportId:  series.ExportId,
		Columns:   []queryColumn{{Name: column}},
		Time: queryTime{
			//the wrapper's SQL is exclusive at both ends, so the start goes back a
			//millisecond for a reading exactly at start to be seen; RFC3339Nano
			//keeps the sub-second digits, which plain RFC3339 would drop
			Start: start.UTC().Add(-time.Millisecond).Format(time.RFC3339Nano),
			End:   end.UTC().Format(time.RFC3339Nano),
		},
		Limit:            1,
		OrderColumnIndex: &timeColumn,
		OrderDirection:   "asc",
	})
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if row[0] == nil {
			if row[1] == nil {
				//the wrapper's no-data marker: this row says the window is free
				continue
			}
			//a value under no instant cannot be placed in the window, so it is a
			//refusal rather than a free window a run would start over unchecked
			return false, fmt.Errorf("unreadable timestamp: a row carries the value %v under no instant", row[1])
		}
		stamp, ok := row[0].(string)
		if !ok {
			//an answer this client cannot read is not an answer: the caller
			//refuses the run rather than starting it unchecked
			return false, fmt.Errorf("unreadable timestamp %v", row[0])
		}
		if _, err = time.Parse(time.RFC3339, stamp); err != nil {
			return false, fmt.Errorf("unreadable timestamp %q: %w", stamp, err)
		}
		return true, nil
	}
	return false, nil
}
