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

package runtime

import (
	"encoding/json"

	platform_connector_lib "github.com/SENERGY-Platform/platform-connector-lib"
)

// eventPublisher is the one thing the runtime needs the connector for. It is an
// interface so that a test can watch what a channel publishes without kafka,
// keycloak and a device repository behind it.
type eventPublisher interface {
	PublishEvent(externalDeviceRef string, externalServiceRef string, value interface{}) error
}

// connectorPublisher publishes exactly like the legacy sendSensorData: the
// value is marshalled into the protocol segment the configuration names, and
// sent with Sync qos under the service's own access token.
//
// The envelope shape is not an implementation detail. It is what the platform's
// marshaller reads, so a migrated channel has to produce the same bytes for the
// same script as the legacy service did.
type connectorPublisher struct {
	connector   *platform_connector_lib.Connector
	segmentName string
}

func (this *connectorPublisher) PublishEvent(externalDeviceRef string, externalServiceRef string, value interface{}) error {
	token, err := this.connector.Security().Access()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	msg := platform_connector_lib.EventMsg{}
	msg[this.segmentName] = string(payload)
	return this.connector.HandleDeviceEventWithAuthToken(token, externalDeviceRef, externalServiceRef, msg, platform_connector_lib.Sync)
}

// deviceStateLogger reports a simulated device as online, the way the legacy
// runtime does in StartDevice. Without it a migrated device shows as offline in
// the platform after the cutover, which is a visible regression rather than a
// cosmetic one: the connection state is what the rest of the platform reads to
// decide whether a device is alive.
//
// Only connect is reported, matching the legacy behaviour exactly — the legacy
// runtime never logs a disconnect, not even when a world is deleted.
type deviceStateLogger interface {
	LogDeviceConnect(id string) error
}
