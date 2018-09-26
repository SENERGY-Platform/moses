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

package connector

import (
	"encoding/json"
	"errors"
	"log"
	"moses/marshaller"
	"strings"
)

type MosesProtocolConnector struct {
	Config   Config
	receiver func(deviceId string, serviceId string, cmdMsg interface{}, responder func(respMsg interface{}))
	producer *Producer
	consumer *RunnerTask
}

func NewMosesProtocolConnector(config Config) (result *MosesProtocolConnector, err error) {
	result = &MosesProtocolConnector{Config: config}
	err = result.init()
	return
}

func (this *MosesProtocolConnector) init() (err error) {
	this.producer, err = InitProducer(this.Config.ZookeeperUrl)
	return
}

func (this *MosesProtocolConnector) Send(deviceId string, serviceId string, transformer marshaller.Marshaller, value interface{}) (err error) {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	protocolparts := []marshaller.ProtocolPart{{Name: "payload", Value: string(b)}}
	formatedEvent, err := transformer.Marshal(protocolparts)
	if err != nil {
		return err
	}
	var eventValue interface{}
	err = json.Unmarshal([]byte(formatedEvent), &eventValue)
	if err != nil {
		return err
	}

	serviceTopic := formatId(serviceId)
	envelope := Envelope{DeviceId: deviceId, ServiceId: serviceId}
	envelope.Value = eventValue
	jsonMsg, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	this.producer.Produce(serviceTopic, string(jsonMsg))
	this.producer.Produce(this.Config.KafkaEventTopic, string(jsonMsg))
	return
}

func (this *MosesProtocolConnector) SetReceiver(receiver func(deviceId string, serviceId string, cmdMsg interface{}, responder func(respMsg interface{}))) {
	this.receiver = receiver
}

func (this *MosesProtocolConnector) Start() (err error) {
	this.consumer = InitKafkaConsumer(this.producer, this.Config.ZookeeperUrl, this.Config.ProtocolTopic, func(msg string) (err error) {
		if this.receiver == nil {
			return errors.New("ERROR: missing receiver in MosesProtocolConnector; use MosesProtocolConnector.SetReceiver()")
		}
		envelope := Envelope{}
		err = json.Unmarshal([]byte(msg), &envelope)
		if err != nil {
			log.Println("ERROR: ", err)
			return nil //ignore marshaling errors --> no repeat; errors would definitely reoccur
		}
		payload, err := json.Marshal(envelope.Value)
		if err != nil {
			log.Println("ERROR: ", err)
			return nil //ignore marshaling errors --> no repeat; errors would definitely reoccur
		}
		protocolmsg := ProtocolMsg{}
		err = json.Unmarshal([]byte(payload), &protocolmsg)
		if err != nil {
			log.Println("ERROR: handle command: ", err.Error()) //ignore marshaling errors --> no repeat; errors would definitely reoccur
			return
		}

		var input interface{}
		if len(protocolmsg.ProtocolParts) != 0 {
			err = json.Unmarshal([]byte(protocolmsg.ProtocolParts[0].Value), &input)
			if err != nil {
				log.Println("WARNING: service input is not json ", err)
				input = protocolmsg.ProtocolParts[0].Value
			}
		}

		this.receiver(envelope.DeviceId, envelope.ServiceId, input, func(respMsg interface{}) {
			output, err := json.Marshal(respMsg)
			if err != nil {
				log.Println("ERROR: while marshaling response")
			} else {
				protocolmsg.ProtocolParts = []marshaller.ProtocolPart{{Name: "payload", Value: string(output)}}
			}
		})
		return
	})
	return
}

func formatId(id string) string {
	return strings.Replace(id, "#", "_", -1)
}
