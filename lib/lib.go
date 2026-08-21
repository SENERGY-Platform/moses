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

		//the connector logs through the service logger instead of its own default
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

	//the per world cutover: a world that exists as an environment is run by the
	//new runtime and must not be started here as well. Both runtimes publish
	//under the same platform device and service ids, so a double start would
	//send every value twice, and two scripts would write the same state.
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

	//one handler for both runtimes, new model first: HandleCommand reports
	//whether the device belongs to an environment, and only if it does not is
	//the command offered to the legacy worlds
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
		//the runtime first: its final state flush needs the environment store,
		//which is closed at the end of this function
		environmentRuntime.Stop()
		staterepo.Stop()
		persistence.Close()
		environments.Close()
	}()
	return nil
}

// migratedWorldIds is the set of legacy world ids that exist as an environment.
// The migration keeps the id of the world it converted, which is what makes this
// comparison possible at all.
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

// environmentNotifier is what the api tells about a stored environment. It
// forwards to the runtime and, on every change, re-runs the one check the per
// world cutover cannot make on its own (see warn).
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

// warn reports the double runs the per world cutover cannot prevent:
//
//   - an environment that references the devices of a world it was not converted
//     from. That world keeps a different id, so it is not skipped.
//   - an environment created while the service runs whose id IS a world id. The
//     skip set is computed at startup, so that world keeps running until the
//     service is restarted.
//
// Both are diagnostics and not guards, on purpose. Which of the two runtimes is
// the intended owner of a device cannot be decided here, and refusing to serve
// would take a service down over a modelling mistake in one document. Checked on
// every change and not only at startup, because the second case only appears
// after a change.
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
		// the zero value of kafka.Compression means uncompressed
		return 0
	case "gzip":
		return kafka.Gzip
	case "snappy":
		return kafka.Snappy
	}
	util.Logger.Warn("unknown compression, falling back to none", "compression", compression)
	return 0
}
