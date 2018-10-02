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
	"moses/marshaller"
	"net/url"
)

func (this *StateRepo) EnsureMosesProtocol(jwt JwtImpersonate) (err error) {
	protocolList, err := this.FindProtocol(jwt, this.Config.KafkaProtocolTopic) //moses protocol name should be same as protocol url
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

func (this *StateRepo) FindProtocol(jwt JwtImpersonate, protocolName string) (result []marshaller.Protocol, err error) {
	err = jwt.GetJSON(this.Config.IotUrl+"/ui/search/others/protocols/"+protocolName+"/10/0", &result)
	return
}

func (this *StateRepo) createMosesProtocol(jwt JwtImpersonate) (err error) {
	protocol := marshaller.Protocol{
		Name:               this.Config.KafkaProtocolTopic,
		ProtocolHandlerUrl: this.Config.KafkaProtocolTopic,
		Desc:               "protocol used by moses (my own smart environment simulator)",
		MsgStructure: []marshaller.MsgSegment{
			{
				Name: "payload",
			},
		},
	}
	err = jwt.PostJSON(this.Config.IotUrl+"/other/protocol", protocol, nil)
	return
}

func (this *StateRepo) GetIotService(jwt Jwt, externalServiceId string) (service marshaller.Service, err error) {
	err = jwt.Impersonate.GetJSON(this.Config.IotUrl+"/service/"+url.PathEscape(externalServiceId), &service)
	return
}
