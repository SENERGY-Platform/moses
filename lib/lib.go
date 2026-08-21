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
	"github.com/IBM/sarama"
	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/api"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/state"
	"github.com/SENERGY-Platform/moses/lib/util"
	platform_connector_lib "github.com/SENERGY-Platform/platform-connector-lib"
	"github.com/SENERGY-Platform/platform-connector-lib/connectionlog"
	"log/slog"
	"strings"
	"time"
)

func New(config config.Config, ctx context.Context) (err error) {

	asyncFlushFrequency, err := time.ParseDuration(config.AsyncFlushFrequency)
	if err != nil {
		return err
	}

	connector, err := platform_connector_lib.New(platform_connector_lib.Config{
		PartitionsNum:            config.KafkaPartitionNum,
		ReplicationFactor:        config.KafkaReplicationFactor,
		FatalKafkaError:          config.FatalKafkaError,
		Protocol:                 config.Protocol,
		KafkaGroupName:           config.KafkaGroupName,
		KafkaUrl:                 config.KafkaUrl,
		AuthExpirationTimeBuffer: config.AuthExpirationTimeBuffer,
		JwtExpiration:            config.JwtExpiration,
		JwtPrivateKey:            config.JwtPrivateKey.Value(),
		JwtIssuer:                config.JwtIssuer,
		AuthClientSecret:         config.AuthClientSecret.Value(),
		AuthClientId:             config.AuthClientId,
		AuthEndpoint:             config.AuthEndpoint,
		DeviceManagerUrl:         config.DeviceManagerUrl,
		DeviceRepoUrl:            config.DeviceRepoUrl,
		KafkaResponseTopic:       config.KafkaResponseTopic,

		//create topics before consuming: if a consumer group joins before its topic exists,
		//the group can become stable with 0 assigned partitions and never receive messages
		InitTopics: true,

		DeviceExpiration:         int32(config.DeviceExpiration),
		DeviceTypeExpiration:     int32(config.DeviceTypeExpiration),
		CharacteristicExpiration: int32(config.CharacteristicExpiration),

		Debug: config.Debug,

		Validate:                  false,
		ValidateAllowMissingField: true,
		ValidateAllowUnknownField: true,

		PublishToPostgres: config.PublishToPostgres,
		PostgresHost:      config.PostgresHost,
		PostgresPort:      config.PostgresPort,
		PostgresUser:      config.PostgresUser.Value(),
		PostgresPw:        config.PostgresPw.Value(),
		PostgresDb:        config.PostgresDb,

		SyncCompression:     getKafkaCompression(config.SyncCompression),
		AsyncCompression:    getKafkaCompression(config.AsyncCompression),
		AsyncFlushFrequency: asyncFlushFrequency,
		AsyncFlushMessages:  int(config.AsyncFlushMessages),
		AsyncPgThreadMax:    int(config.AsyncPgThreadMax),

		KafkaConsumerMinBytes: int(config.KafkaConsumerMinBytes),
		KafkaConsumerMaxBytes: int(config.KafkaConsumerMaxBytes),
		KafkaConsumerMaxWait:  config.KafkaConsumerMaxWait,

		IotCacheTimeout:      config.IotCacheTimeout,
		IotCacheMaxIdleConns: int(config.IotCacheMaxIdleConns),
		IotCacheUrl:          StringToList(config.IotCacheUrls),

		TokenCacheUrl:        StringToList(config.TokenCacheUrls),
		TokenCacheExpiration: int32(config.TokenCacheExpiration),

		DeviceTypeTopic: config.DeviceTypeTopic,

		NotificationUrl:  config.NotificationUrl,
		PermissionsV2Url: config.PermissionsV2Url,

		KafkaTopicConfigs: config.KafkaTopicConfigs,
	})
	if err != nil {
		util.Logger.Error("unable to initialise the connector", attributes.ErrorKey, err)
		return err
	}

	if config.Debug {
		connector.SetKafkaLogger(slog.NewLogLogger(util.Logger.Handler(), slog.LevelDebug))
		connector.IotCache.Debug = true
	}

	err = connector.InitProducer(ctx, []platform_connector_lib.Qos{platform_connector_lib.Sync})
	if err != nil {
		util.Logger.Error("unable to init the kafka producer", attributes.ErrorKey, err)
		return err
	}

	logProducer, err := connector.GetProducer(platform_connector_lib.Sync)
	if err != nil {
		util.Logger.Error("unable to build the connection log", attributes.ErrorKey, err)
		return err
	}
	logger, err := connectionlog.NewWithProducer(logProducer, config.DeviceLogTopic, config.GatewayLogTopic)
	if err != nil {
		util.Logger.Error("unable to build the connection log", attributes.ErrorKey, err)
		return err
	}

	util.Logger.Info("connecting to the database")
	persistence, err := state.NewMongoPersistence(config)
	if err != nil {
		util.Logger.Error("unable to connect to the database", attributes.ErrorKey, err)
		return err
	}

	environments, err := repo.NewMongo(config)
	if err != nil {
		util.Logger.Error("unable to connect to the environment store", attributes.ErrorKey, err)
		return err
	}

	util.Logger.Info("loading states from the database")
	staterepo := &state.StateRepo{Persistence: persistence, Config: config, Connector: connector, StateLogger: logger}
	err = staterepo.Load()
	if err != nil {
		util.Logger.Error("unable to load the state repo", attributes.ErrorKey, err)
		return err
	}

	util.Logger.Info("starting state routines")
	staterepo.Start()

	err = connector.Start(ctx, platform_connector_lib.Sync)
	if err != nil {
		util.Logger.Error("unable to start the protocol", attributes.ErrorKey, err)
		return err
	}

	util.Logger.Info("starting the api", "port", config.ServerPort)

	api.Start(ctx, config, staterepo, environments)
	go func() {
		<-ctx.Done()
		staterepo.Stop()
		persistence.Close()
		environments.Close()
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

func getKafkaCompression(compression string) sarama.CompressionCodec {
	switch strings.ToLower(compression) {
	case "":
		return sarama.CompressionNone
	case "-":
		return sarama.CompressionNone
	case "none":
		return sarama.CompressionNone
	case "gzip":
		return sarama.CompressionGZIP
	case "snappy":
		return sarama.CompressionSnappy
	}
	util.Logger.Warn("unknown compression, falling back to none", "compression", compression)
	return sarama.CompressionNone
}
