/*
 * Copyright 2018 InfAI (CC SES)
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

package connector

import "moses/marshaller"

type Envelope struct {
	DeviceId  string      `json:"device_id,omitempty"`
	ServiceId string      `json:"service_id,omitempty"`
	Value     interface{} `json:"value"`
}

type ProtocolMsg struct {
	WorkerId         string                    `json:"worker_id"`
	TaskId           string                    `json:"task_id"`
	DeviceUrl        string                    `json:"device_url"`
	ServiceUrl       string                    `json:"service_url"`
	ProtocolParts    []marshaller.ProtocolPart `json:"protocol_parts"`
	DeviceInstanceId string                    `json:"device_instance_id"`
	ServiceId        string                    `json:"service_id"`
	OutputName       string                    `json:"output_name"`
	Time             string                    `json:"time"`
	Service          interface{}               `json:"service"`
}
