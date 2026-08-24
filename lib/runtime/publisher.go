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

// eventPublisher is the only thing the runtime needs the connector for.
type eventPublisher interface {
	PublishEvent(externalDeviceRef string, externalServiceRef string, value interface{}) error
}

// connectorPublisher publishes like the legacy sendSensorData. The envelope
// shape is what the platform's marshaller reads, so a migrated channel must
// produce the same bytes for the same script.
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

// deviceStateLogger reports a simulated device as online, as the legacy
// StartDevice does; without it a migrated device shows as offline after the
// cutover. Only connect, matching legacy, which never logs a disconnect.
type deviceStateLogger interface {
	LogDeviceConnect(id string) error
}
