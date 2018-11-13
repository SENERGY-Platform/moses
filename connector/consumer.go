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
	"log"

	"time"

	"github.com/wvanbergen/kafka/consumergroup"
	"github.com/wvanbergen/kazoo-go"
)

const KAFKA_TIMEOUT = 60

func InitKafkaConsumer(producer *Producer, zookeeperUrl string, topic string, msgHandler func(string) error) *RunnerTask {
	return RunTask(func(shouldStop StopCheckFunc) error {
		log.Println("Start KAFKA-Consumer", topic)
		defer log.Println("DEBUG: stop kafka consumer")
		producer.Produce(topic, "topic_init")

		zk, chroot := kazoo.ParseConnectionString(zookeeperUrl)
		kafkaconf := consumergroup.NewConfig()
		kafkaconf.Consumer.Return.Errors = true
		kafkaconf.Zookeeper.Chroot = chroot
		consumerGroupName := topic
		consumer, err := consumergroup.JoinConsumerGroup(
			consumerGroupName,
			[]string{topic},
			zk,
			kafkaconf)

		if err != nil {
			log.Fatal("error in consumergroup.JoinConsumerGroup()", err)
		}

		defer consumer.Close()

		kafkaping := time.NewTicker(time.Second * time.Duration(KAFKA_TIMEOUT/2))
		defer kafkaping.Stop()
		kafkatimout := time.NewTicker(time.Second * time.Duration(KAFKA_TIMEOUT))
		defer kafkatimout.Stop()

		timeout := false

		for {
			if shouldStop() {
				return nil
			}
			select {
			case <-kafkaping.C:
				if timeout {
					producer.Produce(topic, "topic_init")
				}
			case <-kafkatimout.C:
				if timeout {
					log.Fatal("ERROR: kafka missing ping timeout")
				}
				timeout = true
			case errMsg := <-consumer.Errors():
				log.Fatal("kafka consumer error: ", errMsg)
			case msg, ok := <-consumer.Messages():
				if !ok {
					log.Fatal("empty kafka consumer")
				} else {
					if string(msg.Value) != "topic_init" {
						log.Println("DEBUG: receive data: ", msg)
						err = msgHandler(string(msg.Value))
						if err != nil {
							log.Println("WARNING: error while handling kafka msg", err)
						}
					}
					timeout = false
					if err != nil {
						log.Println("ERROR while handling msg:", string(msg.Value))
					} else {
						consumer.CommitUpto(msg)
					}
				}
			}
		}
		return nil
	})
}
