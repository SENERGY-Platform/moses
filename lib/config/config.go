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

package config

import (
	"errors"
	"flag"
	"log"
	"time"

	sb_config_hdl "github.com/SENERGY-Platform/go-service-base/config-hdl"
	sb_config_types "github.com/SENERGY-Platform/go-service-base/config-hdl/types"
	"github.com/segmentio/kafka-go"
)

// Every field carries an explicit env_var tag. The names are NOT derived from
// the field names: the previous loader derived them with its own camel-case
// splitter, and any library deriving them even slightly differently would
// silently rename a deployment variable. TestConfigFieldsMapToTheExpectedEnvironmentVariableNames
// pins the complete set.
//
// Fields holding a credential use sb_config_types.Secret, whose String and
// MarshalJSON return a random placeholder, so the value cannot leak through a
// log line or an error message. Read it with .Value().
type Config struct {
	ServerPort             string                 `json:"server_port" env_var:"SERVER_PORT"`
	LoggerHandler          string                 `json:"logger_handler" env_var:"LOGGER_HANDLER"` //json | text
	LoggerLevel            string                 `json:"logger_level" env_var:"LOGGER_LEVEL"`     //debug | info | warn | error
	WorldCollectionName    string                 `json:"world_collection_name" env_var:"WORLD_COLLECTION_NAME"`
	TemplateCollectionName string                 `json:"template_collection_name" env_var:"TEMPLATE_COLLECTION_NAME"`
	MongoUrl               sb_config_types.Secret `json:"mongo_url" env_var:"MONGO_URL"` //may embed credentials
	MongoTable             string                 `json:"mongo_table" env_var:"MONGO_TABLE"`
	JsTimeout              time.Duration          `json:"js_timeout" env_var:"JS_TIMEOUT"` //json: nanoseconds, env: duration string ("2s")
	ProtocolSegmentName    string                 `json:"protocol_segment_name" env_var:"PROTOCOL_SEGMENT_NAME"`

	// EnvironmentCollectionName holds the environment definitions of the new
	// domain model and StateCollectionName their runtime state. They are separate
	// collections from the legacy worlds, so both models can coexist during the
	// migration.
	EnvironmentCollectionName string `json:"environment_collection_name" env_var:"ENVIRONMENT_COLLECTION_NAME"`
	StateCollectionName       string `json:"state_collection_name" env_var:"STATE_COLLECTION_NAME"`
	// DatasetCollectionName holds the metadata of uploaded datasets; the raw
	// files live in a gridfs bucket named <collection>_content.
	DatasetCollectionName string `json:"dataset_collection_name" env_var:"DATASET_COLLECTION_NAME"`
	// ShareCollectionName holds, per environment, the accounts its devices are
	// shared with. Beside the definition and not in it, see lib/repo/shares.go.
	ShareCollectionName string `json:"share_collection_name" env_var:"SHARE_COLLECTION_NAME"`

	// TimescaleWrapperUrl is where dataset sources with the platform origin
	// fetch real measurements from. Empty disables the origin: affected
	// channels are reported and skipped.
	TimescaleWrapperUrl string `json:"timescale_wrapper_url" env_var:"TIMESCALE_WRAPPER_URL"`

	// StateFlushInterval is how often the runtime writes the changed runtime
	// state of an environment to the database. The runtime keeps the live values
	// in memory and persists them behind this interval, instead of writing on
	// every state.set() the way the legacy runtime does: a channel ticking every
	// second would otherwise turn every simulated value into a database write.
	//
	// The interval is the bound on how much simulated state a crash can lose.
	// json: nanoseconds, env: duration string ("5s").
	StateFlushInterval time.Duration `json:"state_flush_interval" env_var:"STATE_FLUSH_INTERVAL"`

	// PublishWorkers is how many readings a history run or a backfill has in
	// flight at once, which is what multiplies the throughput of a run. A channel
	// is pinned to one worker, so raising it past the number of channels of an
	// environment changes nothing, and every worker holds a queue of jobs
	// carrying a full channel binding. Zero or less is the default of 16, and the
	// count is clamped to 256.
	PublishWorkers int `json:"publish_workers" env_var:"PUBLISH_WORKERS"`

	KafkaUrl           string `json:"kafka_url" env_var:"KAFKA_URL"`
	KafkaResponseTopic string `json:"kafka_response_topic" env_var:"KAFKA_RESPONSE_TOPIC"`
	KafkaGroupName     string `json:"kafka_group_name" env_var:"KAFKA_GROUP_NAME"`
	FatalKafkaError    bool   `json:"fatal_kafka_error" env_var:"FATAL_KAFKA_ERROR"`
	Protocol           string `json:"protocol" env_var:"PROTOCOL"`

	PermissionsV2Url string `json:"permissions_v2_url" env_var:"PERMISSIONS_V2_URL"`
	DeviceManagerUrl string `json:"device_manager_url" env_var:"DEVICE_MANAGER_URL"`
	DeviceRepoUrl    string `json:"device_repo_url" env_var:"DEVICE_REPO_URL"`

	//AuthClientId is the keycloak client id. An OAuth2 client id is a public
	//identifier, not a credential (RFC 6749 section 2.2), so it stays a plain
	//string and remains readable in diagnostics. Only the secret is masked.
	AuthClientId             string                 `json:"auth_client_id" env_var:"AUTH_CLIENT_ID"`
	AuthClientSecret         sb_config_types.Secret `json:"auth_client_secret" env_var:"AUTH_CLIENT_SECRET"`
	AuthExpirationTimeBuffer float64                `json:"auth_expiration_time_buffer" env_var:"AUTH_EXPIRATION_TIME_BUFFER"`
	AuthEndpoint             string                 `json:"auth_endpoint" env_var:"AUTH_ENDPOINT"`

	JwtPrivateKey sb_config_types.Secret `json:"jwt_private_key" env_var:"JWT_PRIVATE_KEY"`
	JwtExpiration int64                  `json:"jwt_expiration" env_var:"JWT_EXPIRATION"`
	JwtIssuer     string                 `json:"jwt_issuer" env_var:"JWT_ISSUER"`

	GatewayLogTopic string `json:"gateway_log_topic" env_var:"GATEWAY_LOG_TOPIC"`
	DeviceLogTopic  string `json:"device_log_topic" env_var:"DEVICE_LOG_TOPIC"`

	Debug bool `json:"debug" env_var:"DEBUG"`

	DeviceExpiration         int64 `json:"device_expiration" env_var:"DEVICE_EXPIRATION"`
	DeviceTypeExpiration     int64 `json:"device_type_expiration" env_var:"DEVICE_TYPE_EXPIRATION"`
	CharacteristicExpiration int64 `json:"characteristic_expiration" env_var:"CHARACTERISTIC_EXPIRATION"`

	KafkaPartitionNum      int `json:"kafka_partition_num" env_var:"KAFKA_PARTITION_NUM"`
	KafkaReplicationFactor int `json:"kafka_replication_factor" env_var:"KAFKA_REPLICATION_FACTOR"`

	PublishToPostgres bool                   `json:"publish_to_postgres" env_var:"PUBLISH_TO_POSTGRES"`
	PostgresHost      string                 `json:"postgres_host" env_var:"POSTGRES_HOST"`
	PostgresPort      int                    `json:"postgres_port" env_var:"POSTGRES_PORT"`
	PostgresUser      sb_config_types.Secret `json:"postgres_user" env_var:"POSTGRES_USER"`
	PostgresPw        sb_config_types.Secret `json:"postgres_pw" env_var:"POSTGRES_PW"`
	PostgresDb        string                 `json:"postgres_db" env_var:"POSTGRES_DB"`

	AsyncPgThreadMax    int64  `json:"async_pg_thread_max" env_var:"ASYNC_PG_THREAD_MAX"`
	AsyncFlushMessages  int64  `json:"async_flush_messages" env_var:"ASYNC_FLUSH_MESSAGES"`
	AsyncFlushFrequency string `json:"async_flush_frequency" env_var:"ASYNC_FLUSH_FREQUENCY"`
	AsyncCompression    string `json:"async_compression" env_var:"ASYNC_COMPRESSION"`
	SyncCompression     string `json:"sync_compression" env_var:"SYNC_COMPRESSION"`

	KafkaConsumerMaxWait  string `json:"kafka_consumer_max_wait" env_var:"KAFKA_CONSUMER_MAX_WAIT"`
	KafkaConsumerMinBytes int64  `json:"kafka_consumer_min_bytes" env_var:"KAFKA_CONSUMER_MIN_BYTES"`
	KafkaConsumerMaxBytes int64  `json:"kafka_consumer_max_bytes" env_var:"KAFKA_CONSUMER_MAX_BYTES"`

	IotCacheUrls         string `json:"iot_cache_urls" env_var:"IOT_CACHE_URLS"`
	IotCacheMaxIdleConns int64  `json:"iot_cache_max_idle_conns" env_var:"IOT_CACHE_MAX_IDLE_CONNS"`
	IotCacheTimeout      string `json:"iot_cache_timeout" env_var:"IOT_CACHE_TIMEOUT"`

	TokenCacheUrls       string `json:"token_cache_urls" env_var:"TOKEN_CACHE_URLS"`
	TokenCacheExpiration int64  `json:"token_cache_expiration" env_var:"TOKEN_CACHE_EXPIRATION"`

	DeviceTypeTopic string `json:"device_type_topic" env_var:"DEVICE_TYPE_TOPIC"`

	NotificationUrl string `json:"notification_url" env_var:"NOTIFICATION_URL"`

	//KafkaTopicConfigs is read as JSON from the environment, for example
	//KAFKA_TOPIC_CONFIGS='{"response":[{"ConfigName":"retention.ms","ConfigValue":"86400000"}]}'
	KafkaTopicConfigs map[string][]kafka.ConfigEntry `json:"kafka_topic_configs" env_var:"KAFKA_TOPIC_CONFIGS"`
}

func LoadConfig() (result Config, err error) {
	return LoadConfigFlag("config")
}

func LoadConfigFlag(configLocationFlag string) (result Config, err error) {
	configLocation := flag.String(configLocationFlag, "config.json", "configuration file")
	flag.Parse()
	return LoadConfigLocation(*configLocation)
}

// LoadConfigLocation reads location as json and then applies the environment
// variables named by the env_var tags on top of it. A missing file, malformed
// json and an unparsable environment value are all returned as an error; none
// of them is silently ignored.
func LoadConfigLocation(location string) (result Config, err error) {
	if location == "" {
		//config_hdl.Load skips empty paths, which would start the service on a
		//zero-valued config instead of reporting the missing file
		err = errors.New("no config location given")
		log.Println("error on config load: ", err)
		return result, err
	}
	err = sb_config_hdl.Load(&result, nil, envTypeParsers, nil, location)
	if err != nil {
		log.Println("error on config load: ", err)
		return result, err
	}
	return result, nil
}
