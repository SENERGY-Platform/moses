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
	"github.com/wvanbergen/kazoo-go"
	"log"
	"time"

	"github.com/Shopify/sarama"
)

type Producer struct {
	producer sarama.AsyncProducer
}

func InitProducer(zkurl string) (producer *Producer, err error) {
	var kz *kazoo.Kazoo
	kz, err = kazoo.NewKazooFromConnectionString(zkurl, nil)
	if err != nil {
		log.Fatal("error in kazoo.NewKazooFromConnectionString()", err)
	}
	broker, err := kz.BrokerList()
	kz.Close()

	if err != nil {
		log.Fatal("error in kz.BrokerList()", err)
	}

	sarama_conf := sarama.NewConfig()
	sarama_conf.Version = sarama.V0_10_0_1
	producer = &Producer{}
	producer.producer, err = sarama.NewAsyncProducer(broker, sarama_conf)
	return
}

func (this *Producer) Produce(topic string, message string) {
	log.Println("DEBUG: Produce", topic, message)
	this.producer.Input() <- &sarama.ProducerMessage{Topic: topic, Key: nil, Value: sarama.StringEncoder(message), Timestamp: time.Now()}
}
