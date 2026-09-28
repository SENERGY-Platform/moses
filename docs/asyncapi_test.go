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

package docs

import (
	"encoding/json"
	"testing"
)

// TestAsyncApiDocIsValidJsonWithTheExpectedChannels pins the embedded spec
// against a stale docs/asyncapi.json: a channel renamed or dropped in
// docs/asyncapi-gen/main.go without regenerating the committed file fails this
// test instead of only showing up as a diff nobody looked at.
func TestAsyncApiDocIsValidJsonWithTheExpectedChannels(t *testing.T) {
	var doc struct {
		Asyncapi string                     `json:"asyncapi"`
		Channels map[string]json.RawMessage `json:"channels"`
	}
	if err := json.Unmarshal(AsyncApiDoc, &doc); err != nil {
		t.Fatalf("AsyncApiDoc is not valid json: %v", err)
	}
	if doc.Asyncapi != "3.0.0" {
		t.Errorf("expected asyncapi version 3.0.0, got %q", doc.Asyncapi)
	}

	expectedChannels := []string{
		"moses",
		"device-types",
		"[device-type-service-topic]",
		"response",
		"device_log",
	}
	for _, name := range expectedChannels {
		if _, ok := doc.Channels[name]; !ok {
			t.Errorf("expected channel %q, got %v", name, channelNames(doc.Channels))
		}
	}
	if len(doc.Channels) != len(expectedChannels) {
		t.Errorf("expected exactly %d channels, got %v", len(expectedChannels), channelNames(doc.Channels))
	}
}

func channelNames(channels map[string]json.RawMessage) []string {
	names := make([]string, 0, len(channels))
	for name := range channels {
		names = append(names, name)
	}
	return names
}
