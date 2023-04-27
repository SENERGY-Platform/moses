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
	"encoding/json"
	"flag"
	"github.com/segmentio/kafka-go"
	"log"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ServerPort             string        `json:"server_port"`
	LogLevel               string        `json:"log_level"`
	WorldCollectionName    string        `json:"world_collection_name"`
	GraphCollectionName    string        `json:"graph_collection_name"`
	TemplateCollectionName string        `json:"template_collection_name"`
	MongoUrl               string        `json:"mongo_url"`
	MongoTable             string        `json:"mongo_table"`
	JsTimeout              time.Duration `json:"js_timeout"`
	ProtocolSegmentName    string        `json:"protocol_segment_name"`

	KafkaUrl           string `json:"kafka_url"`
	KafkaResponseTopic string `json:"kafka_response_topic"`
	KafkaGroupName     string `json:"kafka_group_name"`
	FatalKafkaError    bool   `json:"fatal_kafka_error"` // "true" || "false"; "" -> "true", else -> "false"
	Protocol           string `json:"protocol"`

	PermSearchUrl    string `json:"perm_search_url"`
	DeviceManagerUrl string `json:"device_manager_url"`
	DeviceRepoUrl    string `json:"device_repo_url"`

	AuthClientId             string  `json:"auth_client_id"`     //keycloak-client
	AuthClientSecret         string  `json:"auth_client_secret"` //keycloak-secret
	AuthExpirationTimeBuffer float64 `json:"auth_expiration_time_buffer"`
	AuthEndpoint             string  `json:"auth_endpoint"`

	JwtPrivateKey string `json:"jwt_private_key"`
	JwtExpiration int64  `json:"jwt_expiration"`
	JwtIssuer     string `json:"jwt_issuer"`

	GatewayLogTopic string `json:"gateway_log_topic"`
	DeviceLogTopic  string `json:"device_log_topic"`

	Debug bool `json:"debug"`

	DeviceExpiration         int64 `json:"device_expiration"`
	DeviceTypeExpiration     int64 `json:"device_type_expiration"`
	CharacteristicExpiration int64 `json:"characteristic_expiration"`

	KafkaPartitionNum      int `json:"kafka_partition_num"`
	KafkaReplicationFactor int `json:"kafka_replication_factor"`

	PublishToPostgres bool   `json:"publish_to_postgres"`
	PostgresHost      string `json:"postgres_host"`
	PostgresPort      int    `json:"postgres_port"`
	PostgresUser      string `json:"postgres_user"`
	PostgresPw        string `json:"postgres_pw"`
	PostgresDb        string `json:"postgres_db"`

	AsyncPgThreadMax    int64  `json:"async_pg_thread_max"`
	AsyncFlushMessages  int64  `json:"async_flush_messages"`
	AsyncFlushFrequency string `json:"async_flush_frequency"`
	AsyncCompression    string `json:"async_compression"`
	SyncCompression     string `json:"sync_compression"`

	KafkaConsumerMaxWait  string `json:"kafka_consumer_max_wait"`
	KafkaConsumerMinBytes int64  `json:"kafka_consumer_min_bytes"`
	KafkaConsumerMaxBytes int64  `json:"kafka_consumer_max_bytes"`

	IotCacheUrls         string `json:"iot_cache_urls"`
	IotCacheMaxIdleConns int64  `json:"iot_cache_max_idle_conns"`
	IotCacheTimeout      string `json:"iot_cache_timeout"`

	TokenCacheUrls       string `json:"token_cache_urls"`
	TokenCacheExpiration int64  `json:"token_cache_expiration"`

	DeviceTypeTopic string `json:"device_type_topic"`

	NotificationUrl string `json:"notification_url"`

	KafkaTopicConfigs map[string][]kafka.ConfigEntry `json:"kafka_topic_configs"`
}

func LoadConfig() (result Config, err error) {
	return LoadConfigFlag("config")
}

func LoadConfigFlag(configLocationFlag string) (result Config, err error) {
	configLocation := flag.String(configLocationFlag, "config.json", "configuration file")
	flag.Parse()
	return LoadConfigLocation(*configLocation)
}

func LoadConfigLocation(location string) (result Config, err error) {
	file, err := os.Open(location)
	if err != nil {
		log.Println("error on config load: ", err)
		return result, err
	}
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&result)
	if err != nil {
		log.Println("invalid config json: ", err)
		return result, err
	}
	log.Println("handle environment variables")
	handleEnvironmentVars(&result)
	return
}

var camel = regexp.MustCompile("(^[^A-Z]*|[A-Z]*)([A-Z][^A-Z]+|$)")

func fieldNameToEnvName(s string) string {
	var a []string
	for _, sub := range camel.FindAllStringSubmatch(s, -1) {
		if sub[1] != "" {
			a = append(a, sub[1])
		}
		if sub[2] != "" {
			a = append(a, sub[2])
		}
	}
	return strings.ToUpper(strings.Join(a, "_"))
}

func handleEnvironmentVars(config interface{}) {
	configValue := reflect.Indirect(reflect.ValueOf(config))
	configType := configValue.Type()
	for index := 0; index < configType.NumField(); index++ {
		fieldName := configType.Field(index).Name
		envName := fieldNameToEnvName(fieldName)
		envValue := os.Getenv(envName)
		if envValue != "" {
			log.Println("use environment variable: ", envName, " = ", envValue)
			if configValue.FieldByName(fieldName).Kind() == reflect.Int64 {
				i, _ := strconv.ParseInt(envValue, 10, 64)
				configValue.FieldByName(fieldName).SetInt(i)
			}
			if configValue.FieldByName(fieldName).Kind() == reflect.String {
				configValue.FieldByName(fieldName).SetString(envValue)
			}
			if configValue.FieldByName(fieldName).Kind() == reflect.Bool {
				b, _ := strconv.ParseBool(envValue)
				configValue.FieldByName(fieldName).SetBool(b)
			}
			if configValue.FieldByName(fieldName).Kind() == reflect.Slice {
				val := []string{}
				for _, element := range strings.Split(envValue, ",") {
					val = append(val, strings.TrimSpace(element))
				}
				configValue.FieldByName(fieldName).Set(reflect.ValueOf(val))
			}
			if configValue.FieldByName(fieldName).Kind() == reflect.Map {
				value := map[string]string{}
				for _, element := range strings.Split(envValue, ",") {
					keyVal := strings.Split(element, ":")
					key := strings.TrimSpace(keyVal[0])
					val := strings.TrimSpace(keyVal[1])
					value[key] = val
				}
				configValue.FieldByName(fieldName).Set(reflect.ValueOf(value))
			}

		}
	}
}
