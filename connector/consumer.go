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

import (
	"github.com/SENERGY-Platform/iot-broker-client-lib"
	"log"
)

func InitConsumer(amqpUrl string, topic string, msgHandler func(string) error) (consumer *iot_broker_client_lib.Consumer, err error) {
	consumer, err = iot_broker_client_lib.NewConsumer(amqpUrl, "queue_"+topic, topic, false, 10, func(msg []byte) error {
		return msgHandler(string(msg))
	})
	if err != nil {
		log.Println("ERROR: unable to create amqp consumer", err)
		return
	}
	err = consumer.BindAll()
	if err != nil {
		log.Println("ERROR: unable to bind consumer to all devices", err)
		return
	}
	return
}