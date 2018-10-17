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
	"errors"
	"log"
	"moses/iotmodel"
	"net/url"
)

func (this *StateRepo) EnsureMosesProtocol(jwt JwtImpersonate) (result iotmodel.Protocol, err error) {
	result, err = this.findProtocol(jwt)
	if err != nil {
		err = this.createMosesProtocol(jwt)
	} else {
		return result, err
	}
	if err != nil {
		return result, err
	}
	//create protocol if not found
	return this.findProtocol(jwt)
}

func (this *StateRepo) findProtocol(jwt JwtImpersonate) (result iotmodel.Protocol, err error) {
	protocolList, err := this.findProtocolList(jwt, this.Config.KafkaProtocolTopic) //moses protocol name should be same as protocol url
	if err != nil {
		return result, err
	}
	for _, protocol := range protocolList {
		if protocol.ProtocolHandlerUrl == this.Config.KafkaProtocolTopic {
			return protocol, nil //if protocol is found, we are finished
		}
	}
	//create protocol if not found
	return result, errors.New("no protocol found")
}

func (this *StateRepo) findProtocolList(jwt JwtImpersonate, protocolName string) (result []iotmodel.Protocol, err error) {
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
	if err != nil {
		log.Println("ERROR: unable to get service", err)
	}
	return
}

func (this *StateRepo) GetIotDeviceType(jwt Jwt, id string) (dt iotmodel.DeviceType, err error) {
	err = jwt.Impersonate.GetJSON(this.Config.IotUrl+"/deviceType/"+url.PathEscape(id), &dt)
	if err != nil {
		log.Println("ERROR: unable to get device type", err)
	}
	return
}

func (this *StateRepo) GetDeviceTypesIds(jwt Jwt) (result []string, err error) {
	err = jwt.Impersonate.PostJSON(this.Config.IotUrl+"/query/service", iotmodel.Service{Protocol: iotmodel.Protocol{ProtocolHandlerUrl: this.Config.KafkaProtocolTopic}}, &result)
	if err != nil {
		log.Println("ERROR: unable to query service", err)
	}
	return
}

func (this *StateRepo) GenerateExternalDevice(jwt Jwt, request CreateDeviceByTypeRequest) (device iotmodel.DeviceInstance, err error) {
	deviceInp := iotmodel.DeviceInstance{Name: request.Name, UserTags: []string{"moses"}, DeviceType: request.DeviceTypeId, Url: "moses_will_be_ignored"}
	err = jwt.Impersonate.PostJSON(this.Config.IotUrl+"/deviceInstance", deviceInp, &device)
	if err != nil {
		log.Println("ERROR: unable to create device in iot repository: ", err, device)
	}
	return
}

func (this *StateRepo) DeleteExternalDevice(jwt Jwt, id string) (err error) {
	if id != "" {
		_, err = jwt.Impersonate.Delete(this.Config.IotUrl + "/deviceInstance/" + url.PathEscape(id))
	}
	return
}
