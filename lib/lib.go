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
	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/api"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/runtime"
	"github.com/SENERGY-Platform/moses/lib/state"
	"github.com/SENERGY-Platform/moses/lib/util"
	platform_connector_lib "github.com/SENERGY-Platform/platform-connector-lib"
	"github.com/SENERGY-Platform/platform-connector-lib/connectionlog"
	"github.com/segmentio/kafka-go"
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

		//a consumer group joining before its topic exists can become stable with
		//0 partitions and never receive messages
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

		Logger: util.Logger,
	})
	if err != nil {
		util.Logger.Error("unable to initialise the connector", attributes.ErrorKey, err)
		return err
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

	//per world cutover: both runtimes publish under the same device and service
	//ids, so a world that exists as an environment must not start here too
	staterepo.SkipWorldIds, err = migratedWorldIds(ctx, environments)
	if err != nil {
		util.Logger.Error("unable to determine the migrated worlds", attributes.ErrorKey, err)
		return err
	}

	util.Logger.Info("starting state routines", "skipped_worlds", len(staterepo.SkipWorldIds))
	staterepo.Start()

	util.Logger.Info("starting the environment runtime")
	environmentRuntime := runtime.New(config, environments, environments.States(), connector, logger)
	err = environmentRuntime.Start(ctx)
	if err != nil {
		util.Logger.Error("unable to start the environment runtime", attributes.ErrorKey, err)
		return err
	}

	//new model first: only a device that belongs to no environment is offered
	//to the legacy worlds
	connector.SetAsyncCommandHandler(asyncCommandHandler(config, connector,
		func(externalDeviceRef string, externalServiceRef string, cmdMsg interface{}, responder func(respMsg interface{})) {
			if environmentRuntime.HandleCommand(externalDeviceRef, externalServiceRef, cmdMsg, responder) {
				return
			}
			staterepo.HandleCommand(externalDeviceRef, externalServiceRef, cmdMsg, responder)
		}))

	notifier := &environmentNotifier{runtime: environmentRuntime, staterepo: staterepo}
	notifier.warn()

	err = connector.Start(ctx, platform_connector_lib.Sync)
	if err != nil {
		util.Logger.Error("unable to start the protocol", attributes.ErrorKey, err)
		return err
	}

	util.Logger.Info("starting the api", "port", config.ServerPort)

	api.Start(ctx, config, staterepo, environments, notifier)
	go func() {
		<-ctx.Done()
		//runtime first, its final flush needs the store closed below
		environmentRuntime.Stop()
		staterepo.Stop()
		persistence.Close()
		environments.Close()
	}()
	return nil
}

// migratedWorldIds relies on the migration keeping the id of the world it
// converted.
func migratedWorldIds(ctx context.Context, environments repo.Environments) (map[string]bool, error) {
	all, err := environments.All(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(all))
	for _, env := range all {
		if env.Id == "" {
			continue
		}
		result[env.Id] = true
	}
	return result, nil
}

// environmentNotifier forwards api changes to the runtime and re-runs warn().
type environmentNotifier struct {
	runtime   *runtime.Runtime
	staterepo *state.StateRepo
}

func (this *environmentNotifier) Reload(id string) {
	this.runtime.Reload(id)
	this.warn()
}

func (this *environmentNotifier) SetState(id string, change repo.StateChange) error {
	return this.runtime.SetState(id, change)
}

func (this *environmentNotifier) Remove(id string) {
	this.runtime.Remove(id)
	this.warn()
}

// warn reports the two double runs the per world cutover cannot prevent: an
// environment referencing the devices of a world it was not converted from, and
// an environment created after startup whose id is a world id (the skip set is
// computed once). Diagnostics, not guards - which runtime owns a device cannot
// be decided here, and refusing to serve would take the service down over a
// modelling mistake in one document.
func (this *environmentNotifier) warn() {
	legacy := this.staterepo.ExternalRefWorldIds()
	if len(legacy) == 0 {
		return
	}
	for _, ref := range this.runtime.ExternalDeviceRefs() {
		worldId, shared := legacy[ref]
		if !shared {
			continue
		}
		util.Logger.Warn("a running legacy world and an environment both act on this platform device, its values are sent twice",
			"device_ref", ref, "world", worldId)
	}
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

func getKafkaCompression(compression string) kafka.Compression {
	switch strings.ToLower(compression) {
	case "", "-", "none":
		return 0 //the zero value means uncompressed
	case "gzip":
		return kafka.Gzip
	case "snappy":
		return kafka.Snappy
	}
	util.Logger.Warn("unknown compression, falling back to none", "compression", compression)
	return 0
}
