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

package lib

import (
	"context"
	"github.com/SENERGY-Platform/moses/lib/api"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/state"
	platform_connector_lib "github.com/SENERGY-Platform/platform-connector-lib"
	"log"
	"os"
	"strings"
)

func New(config config.Config, ctx context.Context) (err error) {
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

	log.Println("connect to database")
	persistence, err := state.NewMongoPersistence(config)
	if err != nil {
		log.Println("ERROR: unable to connect to database: ", err)
		return err
	}

	log.Println("load states from database")
	staterepo := &state.StateRepo{Persistence: persistence, Config: config, Connector: connector}
	err = staterepo.Load()
	if err != nil {
		log.Println("ERROR: unable to load state repo: ", err)
		return err
	}

	log.Println("start state routines")
	staterepo.Start()

	err = connector.Start()
	if err != nil {
		log.Println("ERROR: unable to start protocol: ", err)
		return err
	}

	log.Println("start api on port: ", config.ServerPort)

	api.Start(ctx, config, staterepo)
	go func() {
		<-ctx.Done()
		connector.Stop()
		staterepo.Stop()
		persistence.Close()
	}()
	return nil
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
