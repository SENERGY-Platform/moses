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

package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/SENERGY-Platform/moses/lib/config"
	platform_connector_lib "github.com/SENERGY-Platform/platform-connector-lib"
	"github.com/SENERGY-Platform/platform-connector-lib/connectionlog"
	"github.com/SENERGY-Platform/platform-connector-lib/model"

	"github.com/swaggest/go-asyncapi/reflector/asyncapi-2.4.0"
	"github.com/swaggest/go-asyncapi/spec-2.4.0"
)

//go:generate go run main.go

func main() {
	configLocation := flag.String("config", "../../config.json", "configuration file")
	flag.Parse()

	conf, err := config.LoadConfigLocation(*configLocation)
	if err != nil {
		log.Fatal("ERROR: unable to load config", err)
	}

	asyncAPI := spec.AsyncAPI{}
	asyncAPI.Info.Title = "Moses"
	asyncAPI.Info.Description = "moses talks to the platform through platform-connector-lib; a topic in '[]' is a placeholder"

	asyncAPI.AddServer("kafka", spec.Server{
		URL:      conf.KafkaUrl,
		Protocol: "kafka",
	})

	reflector := asyncapi.Reflector{}
	reflector.Schema = &asyncAPI

	mustNotFail := func(err error) {
		if err != nil {
			panic(err.Error())
		}
	}

	mustNotFail(reflector.AddChannel(asyncapi.ChannelInfo{
		Name: conf.Protocol,
		BaseChannelItem: &spec.ChannelItem{
			Servers:     []string{"kafka"},
			Description: "topic configured by config.Protocol; the platform's commands for this connector's protocol handler, consumer group config.KafkaGroupName",
		},
		Subscribe: &asyncapi.MessageSample{
			MessageEntity: spec.MessageEntity{
				Name:  "ProtocolMsg",
				Title: "ProtocolMsg",
			},
			MessageSample: new(model.ProtocolMsg),
		},
	}))

	mustNotFail(reflector.AddChannel(asyncapi.ChannelInfo{
		Name: conf.DeviceTypeTopic,
		BaseChannelItem: &spec.ChannelItem{
			Servers:     []string{"kafka"},
			Description: "topic configured by config.DeviceTypeTopic; invalidates the local iot cache when a device type changes upstream",
		},
		Subscribe: &asyncapi.MessageSample{
			MessageEntity: spec.MessageEntity{
				Name:  "DeviceTypeCommand",
				Title: "DeviceTypeCommand",
			},
			MessageSample: new(platform_connector_lib.DeviceTypeCommand),
		},
	}))

	mustNotFail(reflector.AddChannel(asyncapi.ChannelInfo{
		Name: "[device-type-service-topic]",
		BaseChannelItem: &spec.ChannelItem{
			Servers:     []string{"kafka"},
			Description: "one topic per device-type service, named by platform-connector-lib's ServiceIdToTopic (the service id with '#' and ':' replaced by '_'); carries one reading for one device",
		},
		Publish: &asyncapi.MessageSample{
			MessageEntity: spec.MessageEntity{
				Name:  "Envelope",
				Title: "Envelope",
			},
			MessageSample: new(model.Envelope),
		},
	}))

	mustNotFail(reflector.AddChannel(asyncapi.ChannelInfo{
		Name: conf.KafkaResponseTopic,
		BaseChannelItem: &spec.ChannelItem{
			Servers:     []string{"kafka"},
			Description: "topic configured by config.KafkaResponseTopic; moses's answer to a command consumed from the protocol topic, with Response.Output filled. Overridden per command by metadata.response_to, which may instead be an http(s) callback outside this kafka server",
		},
		Publish: &asyncapi.MessageSample{
			MessageEntity: spec.MessageEntity{
				Name:  "ProtocolMsg",
				Title: "ProtocolMsg",
			},
			MessageSample: new(model.ProtocolMsg),
		},
	}))

	mustNotFail(reflector.AddChannel(asyncapi.ChannelInfo{
		Name: conf.DeviceLogTopic,
		BaseChannelItem: &spec.ChannelItem{
			Servers:     []string{"kafka"},
			Description: "topic configured by config.DeviceLogTopic; signals a simulated device as connected, sent once per environment device at runtime start. Moses never logs a disconnect",
		},
		Publish: &asyncapi.MessageSample{
			MessageEntity: spec.MessageEntity{
				Name:  "DeviceLog",
				Title: "DeviceLog",
			},
			MessageSample: new(connectionlog.DeviceLog),
		},
	}))

	buff, err := reflector.Schema.MarshalJSON()
	mustNotFail(err)

	fmt.Println(string(buff))
	mustNotFail(os.WriteFile("asyncapi.json", buff, 0o600))
}
