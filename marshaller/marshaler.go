/*
 * Copyright 2018 SENERGY Team
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

package marshaller

import (
	"encoding/json"
	"log"
	"moses/iotmodel"
)

type Marshaller struct {
	Service iotmodel.Service `json:"service" bson:"service"`
}

type ProtocolPart struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (this *Marshaller) Marshal(event []ProtocolPart) (result string, err error) {
	formated, err := this.toInternOutput(event)
	if err != nil {
		return result, err
	}
	byteResult, err := json.Marshal(formated)
	if err != nil {
		return result, err
	}
	result = string(byteResult)
	return
}

//could be a lot simpler, but to ensure same behavior as normal platform connectors there code is reused
func (this *Marshaller) toInternOutput(eventMsg []ProtocolPart) (result FormatedOutput, err error) {
	result = map[string]interface{}{}
	for _, output := range eventMsg {
		for _, serviceOutput := range this.Service.Output {
			if serviceOutput.MsgSegment.Name == output.Name {
				parsedOutput, err := ParseFromJson(serviceOutput.Type, output.Value)
				if err != nil {
					log.Println("error on parsing")
					return result, err
				}
				outputInterface, err := FormatToJsonStruct(parsedOutput)
				if err != nil {
					return result, err
				}
				parsedOutput.Name = serviceOutput.Name
				result[serviceOutput.Name] = outputInterface
			}
		}
	}
	return
}
