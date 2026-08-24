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

// Package dataset parses uploaded timeseries files. Parsing happens at upload
// time so a broken file is refused with a line number instead of a channel
// that silently plays nothing.
package dataset

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// MaxRows bounds what a single upload may expand to in memory. Two million
// rows is two years in 30 second resolution.
const MaxRows = 2_000_000

// Point is one sample. Unix seconds, because none of the supported inputs
// carries sub-second resolution.
type Point struct {
	Unix  int64   `json:"unix" bson:"unix"`
	Value float64 `json:"value" bson:"value"`
}

// Series is one value column of an upload. Gaps are allowed - an empty cell
// skips the point - so two series of one file may differ in length.
type Series struct {
	Name   string  `json:"name" bson:"name"`
	Points []Point `json:"points" bson:"points"`
}

func (this Series) From() int64 { return this.Points[0].Unix }
func (this Series) To() int64   { return this.Points[len(this.Points)-1].Unix }

// ParseCSV reads a timeseries file: a header line, a time column first, one or
// more named value columns after it.
//
// The dialect is detected, not declared, because the files come from foreign
// exports: semicolon or comma separated, decimal comma or point, and the time
// as RFC3339, "2006-01-02 15:04[:05]", "02.01.2006 15:04[:05]" or unix
// seconds. A timestamp without an offset is interpreted in loc - german energy
// exports carry local time, and reading it as UTC shifts every value by an
// hour or two, which is the silent corruption this parser exists to refuse.
func ParseCSV(content []byte, loc *time.Location) ([]Series, error) {
	if loc == nil {
		return nil, fmt.Errorf("a timezone is required to interpret timestamps without an offset")
	}
	lines := splitLines(content)
	if len(lines) < 3 {
		return nil, fmt.Errorf("expected a header line and at least two data rows, got %d lines", len(lines))
	}
	delimiter := detectDelimiter(lines[0])
	header := strings.Split(lines[0], delimiter)
	if len(header) < 2 {
		return nil, fmt.Errorf("the header names no value column (delimiter %q)", delimiter)
	}
	if len(lines)-1 > MaxRows {
		return nil, fmt.Errorf("the file has %d rows, the limit is %d", len(lines)-1, MaxRows)
	}

	result := make([]Series, len(header)-1)
	for i := range result {
		name := strings.TrimSpace(header[i+1])
		if name == "" {
			return nil, fmt.Errorf("value column %d has no name in the header", i+1)
		}
		result[i] = Series{Name: name}
	}

	lastUnix := int64(math.MinInt64)
	for row, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, delimiter)
		if len(fields) != len(header) {
			return nil, fmt.Errorf("line %d has %d fields, the header has %d", row+2, len(fields), len(header))
		}
		unix, err := parseTime(strings.TrimSpace(fields[0]), loc)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", row+2, err)
		}
		if unix <= lastUnix {
			return nil, fmt.Errorf("line %d: timestamps must be strictly increasing", row+2)
		}
		lastUnix = unix
		for i := range result {
			cell := strings.TrimSpace(fields[i+1])
			if cell == "" {
				continue
			}
			value, err := parseNumber(cell)
			if err != nil {
				return nil, fmt.Errorf("line %d, column %q: %w", row+2, result[i].Name, err)
			}
			result[i].Points = append(result[i].Points, Point{Unix: unix, Value: value})
		}
	}

	for _, series := range result {
		if len(series.Points) < 2 {
			return nil, fmt.Errorf("column %q has %d values, replay needs at least 2", series.Name, len(series.Points))
		}
	}
	return result, nil
}

func splitLines(content []byte) []string {
	text := string(bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF})) //byte order mark
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	return lines
}

// detectDelimiter: a semicolon in the header wins, because a comma there may
// be a decimal comma further down but a semicolon is never anything else.
func detectDelimiter(header string) string {
	if strings.Contains(header, ";") {
		return ";"
	}
	return ","
}

func parseNumber(cell string) (float64, error) {
	value, err := strconv.ParseFloat(cell, 64)
	if err == nil {
		return value, nil
	}
	//decimal comma; thousands separators are not supported, they are ambiguous
	value, err = strconv.ParseFloat(strings.Replace(cell, ",", ".", 1), 64)
	if err != nil {
		return 0, fmt.Errorf("not a number: %q", cell)
	}
	return value, nil
}

var offsetless = []string{
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"02.01.2006 15:04:05",
	"02.01.2006 15:04",
}

func parseTime(cell string, loc *time.Location) (int64, error) {
	if t, err := time.Parse(time.RFC3339, cell); err == nil {
		return t.Unix(), nil
	}
	for _, layout := range offsetless {
		if t, err := time.ParseInLocation(layout, cell, loc); err == nil {
			return t.Unix(), nil
		}
	}
	//unix seconds; the lower bound rejects years and row numbers
	if unix, err := strconv.ParseInt(cell, 10, 64); err == nil && unix > 100_000_000 {
		return unix, nil
	}
	return 0, fmt.Errorf("unreadable timestamp %q", cell)
}
