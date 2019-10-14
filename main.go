/*
 * Copyright 2019 InfAI (CC SES)
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
	"github.com/SENERGY-Platform/moses/lib"
	platform_connector_lib "github.com/SENERGY-Platform/platform-connector-lib"
	"log"
	"os"
	"strings"
	"time"
)

func main() {
	log.Println("load config")
	config, err := lib.LoadConfig()
	if err != nil {
		log.Fatal("unable to load config: ", err)
	}

	log.Println("init protocol handler")
	connector := platform_connector_lib.New(platform_connector_lib.Config{
		FatalKafkaError:          config.FatalKafkaError,
		Protocol:                 config.Protocol,
		KafkaGroupName:           config.KafkaGroupName,
		ZookeeperUrl:             config.ZookeeperUrl,
		AuthExpirationTimeBuffer: config.AuthExpirationTimeBuffer,
		JwtExpiration:            config.JwtExpiration,
		JwtPrivateKey:            config.JwtPrivateKey,
		JwtIssuer:                config.JwtIssuer,
		AuthClientSecret:         config.AuthClientSecret,
		AuthClientId:             config.AuthClientId,
		AuthEndpoint:             config.AuthEndpoint,
		DeviceManagerUrl:         config.DeviceManagerUrl,
		DeviceRepoUrl:            config.DeviceRepoUrl,
		KafkaResponseTopic:       config.KafkaResponseTopic,

		IotCacheUrl:          StringToList(config.IotCacheUrls),
		DeviceExpiration:     int32(config.DeviceExpiration),
		DeviceTypeExpiration: int32(config.DeviceTypeExpiration),

		TokenCacheUrl:        StringToList(config.TokenCacheUrls),
		TokenCacheExpiration: int32(config.TokenCacheExpiration),

		SyncKafka:           config.SyncKafka,
		SyncKafkaIdempotent: config.SyncKafkaIdempotent,
		Debug:               config.Debug,
	})

	if config.Debug {
		connector.SetKafkaLogger(log.New(os.Stdout, "[CONNECTOR-KAFKA] ", 0))
		connector.IotCache.Debug = true
	}

	time.Sleep(5 * time.Second) //wait for routing tables in cluster

	log.Println("connect to database")
	persistence, err := lib.NewMongoPersistence(config)
	if err != nil {
		log.Fatal("unable to connect to database: ", err)
	}

	log.Println("load states from database")
	staterepo := &lib.StateRepo{Persistence: persistence, Config: config, Connector: connector}
	err = staterepo.Load()
	if err != nil {
		log.Fatal("unable to load state repo: ", err)
	}

	log.Println("start state routines")
	staterepo.Start()

	err = connector.Start()
	if err != nil {
		log.Fatal("unable to start protocol: ", err)
	}

	log.Println("start api on port: ", config.ServerPort)
	lib.StartApi(config, staterepo)

}

func StringToList(str string) []string {
	temp := strings.Split(str, ",")
	result := []string{}
	for _, e := range temp {
		trimmed := strings.TrimSpace(e)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
