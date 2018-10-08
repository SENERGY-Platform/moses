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

package main

import (
	"github.com/google/uuid"
	"log"
	"moses/iotmodel"
	"net/url"
)

func (this *StateRepo) EnsureMosesProtocol(jwt JwtImpersonate) (err error) {
	protocolList, err := this.findProtocol(jwt, this.Config.KafkaProtocolTopic) //moses protocol name should be same as protocol url
	if err != nil {
		return err
	}
	for _, protocol := range protocolList {
		if protocol.ProtocolHandlerUrl == this.Config.KafkaProtocolTopic {
			return nil //if protocol is found, we are finished
		}
	}

	//create protocol if not found
	return this.createMosesProtocol(jwt)
}

func (this *StateRepo) findProtocol(jwt JwtImpersonate, protocolName string) (result []iotmodel.Protocol, err error) {
	err = jwt.GetJSON(this.Config.IotUrl+"/ui/search/others/protocols/"+protocolName+"/10/0", &result)
	return
}

func (this *StateRepo) createMosesProtocol(jwt JwtImpersonate) (err error) {
	protocol := iotmodel.Protocol{
		Name:               this.Config.KafkaProtocolTopic,
		ProtocolHandlerUrl: this.Config.KafkaProtocolTopic,
		Desc:               "protocol used by moses (my own smart environment simulator)",
		MsgStructure: []iotmodel.MsgSegment{
			{
				Name: "payload",
			},
		},
	}
	err = jwt.PostJSON(this.Config.IotUrl+"/other/protocol", protocol, nil)
	return
}

func (this *StateRepo) GetIotService(jwt Jwt, externalServiceId string) (service iotmodel.Service, err error) {
	err = jwt.Impersonate.GetJSON(this.Config.IotUrl+"/service/"+url.PathEscape(externalServiceId), &service)
	return
}

func (this *StateRepo) GetIotDeviceType(jwt Jwt, id string) (dt iotmodel.DeviceType, err error) {
	err = jwt.Impersonate.GetJSON(this.Config.IotUrl+"/deviceType/"+url.PathEscape(id), &dt)
	return
}

func (this *StateRepo) GetDeviceTypesIds(jwt Jwt) (result []string, err error) {
	err = jwt.Impersonate.PostJSON(this.Config.IotUrl+"/query/service", iotmodel.Service{Protocol: iotmodel.Protocol{ProtocolHandlerUrl: this.Config.KafkaProtocolTopic}}, &result)
	return
}

func (this *StateRepo) GenerateExternalDevice(jwt Jwt, request CreateDeviceByTypeRequest) (device iotmodel.DeviceInstance, err error) {
	uri, err := uuid.NewRandom()
	if err != nil {
		return device, err
	}
	device = iotmodel.DeviceInstance{Name: request.Name, UserTags: []string{"moses"}, Url: uri.String(), DeviceType: request.DeviceTypeId}
	var response interface{}
	err = jwt.Impersonate.PostJSON(this.Config.IotUrl+"/deviceInstance", device, &response)
	if err != nil {
		log.Println("ERROR: unable to create device in iot repository: ", err, response)
	}
	return
}
