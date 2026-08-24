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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/SENERGY-Platform/moses/lib/dataset"
)

const requestTimeout = 30 * time.Second

// maxPoints bounds one fetch the way dataset.MaxRows bounds one upload.
const maxPoints = dataset.MaxRows

type Client struct {
	BaseUrl string
	client  *http.Client
}

func New(baseUrl string) *Client {
	return &Client{BaseUrl: baseUrl, client: &http.Client{Timeout: requestTimeout}}
}

type queryTime struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type queryColumn struct {
	Name string `json:"name"`
}

type queryElement struct {
	DeviceId  string        `json:"deviceId"`
	ServiceId string        `json:"serviceId"`
	Columns   []queryColumn `json:"columns"`
	Time      queryTime     `json:"time"`
	Limit     int           `json:"limit"`
}

// Fetch loads one column of one service's timeseries for [start, end). The
// time_format parameter pins the wrapper's timestamp rendering to RFC3339, so
// this client does not depend on the wrapper's default.
func (this *Client) Fetch(token string, deviceId string, serviceId string, column string, start time.Time, end time.Time) ([]dataset.Point, error) {
	body, err := json.Marshal([]queryElement{{
		DeviceId:  deviceId,
		ServiceId: serviceId,
		Columns:   []queryColumn{{Name: column}},
		Time: queryTime{
			Start: start.UTC().Format(time.RFC3339),
			End:   end.UTC().Format(time.RFC3339),
		},
		Limit: maxPoints,
	}})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequest(http.MethodPost, this.BaseUrl+"/queries?format=per_query&time_format="+time.RFC3339, bytes.NewReader(body))
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
		return nil, fmt.Errorf("the timescale-wrapper answered %d: %s", response.StatusCode, message)
	}

	//per_query: one row array per request element, rows are [timestamp, value]
	series := [][][2]interface{}{}
	if err = json.NewDecoder(response.Body).Decode(&series); err != nil {
		return nil, fmt.Errorf("unreadable wrapper response: %w", err)
	}
	if len(series) != 1 {
		return nil, fmt.Errorf("expected one series for one request element, got %d", len(series))
	}

	points := make([]dataset.Point, 0, len(series[0]))
	for _, row := range series[0] {
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
	if len(deduplicated) < 2 {
		return nil, fmt.Errorf("the window holds %d usable measurements, replay needs at least 2", len(deduplicated))
	}
	return deduplicated, nil
}
