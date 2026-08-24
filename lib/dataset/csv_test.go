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

package dataset

import (
	"strings"
	"testing"
	"time"
)

var berlin, _ = time.LoadLocation("Europe/Berlin")

func TestParsesAPlainEnglishCSV(t *testing.T) {
	series, err := ParseCSV([]byte(
		"time,power\n2026-01-05 00:00,1.5\n2026-01-05 00:15,2.25\n"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || series[0].Name != "power" {
		t.Fatalf("expected one column named power, got %+v", series)
	}
	want := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC).Unix()
	if series[0].Points[0] != (Point{Unix: want, Value: 1.5}) {
		t.Errorf("got %+v", series[0].Points[0])
	}
}

// The reason this parser exists: a german export read naively produces values
// that are wrong by a decimal shift and timestamps wrong by an hour.
func TestParsesAGermanEnergyExport(t *testing.T) {
	series, err := ParseCSV([]byte(
		"Zeitstempel;Wirkleistung kW\n05.01.2026 00:00;1,5\n05.01.2026 00:15;22,25\n"), berlin)
	if err != nil {
		t.Fatal(err)
	}
	if series[0].Name != "Wirkleistung kW" {
		t.Errorf("column name: %q", series[0].Name)
	}
	if series[0].Points[0].Value != 1.5 || series[0].Points[1].Value != 22.25 {
		t.Errorf("decimal comma was misread: %+v", series[0].Points)
	}
	//midnight Berlin in january is 23:00 UTC the day before
	want := time.Date(2026, 1, 4, 23, 0, 0, 0, time.UTC).Unix()
	if series[0].Points[0].Unix != want {
		t.Errorf("local time was not interpreted in the given zone: got %d, want %d", series[0].Points[0].Unix, want)
	}
}

func TestParsesRFC3339AndUnixSeconds(t *testing.T) {
	series, err := ParseCSV([]byte(
		"t,v\n2026-01-05T10:00:00+01:00,1\n1767611700,2\n"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if series[0].Points[0].Unix != 1767603600 || series[0].Points[1].Unix != 1767611700 {
		t.Errorf("got %+v", series[0].Points)
	}
}

func TestMultipleColumnsAndGaps(t *testing.T) {
	series, err := ParseCSV([]byte(
		"time;strom;gas\n2026-01-05 00:00;1;10\n2026-01-05 00:15;;11\n2026-01-05 00:30;3;12\n"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 2 {
		t.Fatalf("expected two columns, got %d", len(series))
	}
	if len(series[0].Points) != 2 || len(series[1].Points) != 3 {
		t.Errorf("an empty cell has to skip only its own point: %d and %d", len(series[0].Points), len(series[1].Points))
	}
}

func TestHandlesBOMAndCRLF(t *testing.T) {
	_, err := ParseCSV([]byte("\xEF\xBB\xBFtime,v\r\n2026-01-05 00:00,1\r\n2026-01-05 00:15,2\r\n"), time.UTC)
	if err != nil {
		t.Errorf("a windows export has to parse: %v", err)
	}
}

func TestRefusalsCarryTheLineNumber(t *testing.T) {
	for _, tc := range []struct{ name, content, fragment string }{
		{"unreadable timestamp", "t,v\nkaputt,1\n2026-01-05 00:15,2\n", "line 2"},
		{"not a number", "t,v\n2026-01-05 00:00,1\n2026-01-05 00:15,zwei\n", "line 3"},
		{"time going backwards", "t,v\n2026-01-05 00:15,1\n2026-01-05 00:00,2\n", "strictly increasing"},
		{"duplicate timestamp", "t,v\n2026-01-05 00:00,1\n2026-01-05 00:00,2\n", "strictly increasing"},
		{"field count", "t,v\n2026-01-05 00:00,1,extra\n2026-01-05 00:15,2\n", "line 2"},
		{"no value column", "nurzeit\n1\n2\n", "no value column"},
		{"too short", "t,v\n2026-01-05 00:00,1\n", "at least two data rows"},
		{"one point in a column", "t;a;b\n2026-01-05 00:00;1;1\n2026-01-05 00:15;;2\n", "at least 2"},
	} {
		_, err := ParseCSV([]byte(tc.content), time.UTC)
		if err == nil || !strings.Contains(err.Error(), tc.fragment) {
			t.Errorf("%s: expected %q, got %v", tc.name, tc.fragment, err)
		}
	}
}

func TestNilLocationIsRefused(t *testing.T) {
	if _, err := ParseCSV([]byte("t,v\n1,1\n2,2\n"), nil); err == nil {
		t.Error("a nil location has to be refused, it would guess the meaning of local time")
	}
}
