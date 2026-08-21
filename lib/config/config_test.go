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

package config

// These tests pin the OBSERVABLE behaviour of the config loader. They started
// out as a snapshot of the hand-rolled reflection loader, taken so that it
// could be replaced by go-service-base/config-hdl without silently changing how
// deployments are configured.
//
// The loader has now been replaced. Where a test previously documented a defect
// of the old loader ("CURRENT BEHAVIOUR, BUG"), it now documents the corrected
// behaviour and says what it used to assert, so that a regression back to the
// old semantics is visible in the diff.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	sb_config_hdl "github.com/SENERGY-Platform/go-service-base/config-hdl"
	sb_config_types "github.com/SENERGY-Platform/go-service-base/config-hdl/types"
	"github.com/segmentio/kafka-go"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// writeConfigFile writes content to a file inside the test's temp dir and
// returns its path.
func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	location := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(location, []byte(content), 0600); err != nil {
		t.Fatalf("unable to write temp config: %v", err)
	}
	return location
}

// configEnvVarNames returns the field name -> env_var tag mapping of Config.
// The new loader reads the tag, it does not derive the name from the field, so
// the tag is what the tests have to inspect.
func configEnvVarNames() map[string]string {
	configType := reflect.TypeOf(Config{})
	names := make(map[string]string, configType.NumField())
	for index := 0; index < configType.NumField(); index++ {
		field := configType.Field(index)
		names[field.Name] = field.Tag.Get("env_var")
	}
	return names
}

// unsetEnv removes the named variables for the duration of the test. t.Setenv
// registers the restore of the original value (or its absence) and also guards
// against t.Parallel; the following Unsetenv makes the variable genuinely
// absent, which is what the loader distinguishes.
//
// Setting a variable to "" is NOT a way to neutralize it any more: the new
// loader uses os.LookupEnv, so an exported empty value is applied -- it clears
// a string field and is a hard parse error for a numeric or boolean one.
func unsetEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unable to unset %s: %v", name, err)
		}
	}
}

// neutralizeConfigEnv makes every environment variable the loader looks at
// absent, so a test is hermetic no matter what the developer or CI runner has
// exported.
func neutralizeConfigEnv(t *testing.T) {
	t.Helper()
	for _, envName := range configEnvVarNames() {
		unsetEnv(t, envName)
	}
}

// probeEnvNames are the variables of the local probe structs further down.
var probeEnvNames = []string{
	"STRING_FIELD", "INT64_FIELD", "INT_FIELD", "FLOAT64_FIELD", "BOOL_FIELD",
	"SLICE_FIELD", "MAP_FIELD", "INT_MAP_FIELD", "PLAIN_FIELD", "SECRET_FIELD",
}

func neutralizeProbeEnv(t *testing.T) {
	t.Helper()
	unsetEnv(t, probeEnvNames...)
}

// loadProbe applies the environment to target with exactly the parser set the
// service uses, and without reading any file.
func loadProbe(t *testing.T, target any) error {
	t.Helper()
	return sb_config_hdl.Load(target, nil, envTypeParsers, nil)
}

// captureStdout redirects os.Stdout while f runs and returns what was written.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("unable to create pipe: %v", err)
	}
	original := os.Stdout
	os.Stdout = writer
	collected := make(chan string, 1)
	go func() {
		buffer := new(bytes.Buffer)
		_, _ = io.Copy(buffer, reader)
		collected <- buffer.String()
	}()
	defer func() {
		os.Stdout = original
		_ = writer.Close()
		_ = reader.Close()
	}()
	f()
	os.Stdout = original
	_ = writer.Close()
	return <-collected
}

// ---------------------------------------------------------------------------
// LoadConfigLocation
// ---------------------------------------------------------------------------

func TestLoadConfigLocationPopulatesFieldsFromTheJsonFile(t *testing.T) {
	neutralizeConfigEnv(t)
	location := writeConfigFile(t, `{
		"server_port": "8080",
		"mongo_url": "mongodb://localhost:27017",
		"js_timeout": 5000000000,
		"debug": true,
		"jwt_expiration": 3600,
		"auth_expiration_time_buffer": 0.5,
		"postgres_port": 5432,
		"kafka_topic_configs": {"topic-a": [{"ConfigName": "retention.ms", "ConfigValue": "-1"}]}
	}`)

	config, err := LoadConfigLocation(location)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if config.ServerPort != "8080" {
		t.Errorf("ServerPort: expected \"8080\", got %q", config.ServerPort)
	}
	// MongoUrl is a Secret now, so the plaintext is only reachable via Value().
	if config.MongoUrl.Value() != "mongodb://localhost:27017" {
		t.Errorf("MongoUrl: expected the url from the file, got %q", config.MongoUrl.Value())
	}
	if config.JsTimeout != 5*time.Second {
		t.Errorf("JsTimeout: expected 5s, got %v", config.JsTimeout)
	}
	if config.Debug != true {
		t.Errorf("Debug: expected true, got %v", config.Debug)
	}
	if config.JwtExpiration != 3600 {
		t.Errorf("JwtExpiration: expected 3600, got %v", config.JwtExpiration)
	}
	if config.AuthExpirationTimeBuffer != 0.5 {
		t.Errorf("AuthExpirationTimeBuffer: expected 0.5, got %v", config.AuthExpirationTimeBuffer)
	}
	if config.PostgresPort != 5432 {
		t.Errorf("PostgresPort: expected 5432, got %v", config.PostgresPort)
	}
	expectedTopicConfigs := map[string][]kafka.ConfigEntry{
		"topic-a": {{ConfigName: "retention.ms", ConfigValue: "-1"}},
	}
	if !reflect.DeepEqual(config.KafkaTopicConfigs, expectedTopicConfigs) {
		t.Errorf("KafkaTopicConfigs: expected %v, got %v", expectedTopicConfigs, config.KafkaTopicConfigs)
	}
}

func TestLoadConfigLocationLeavesFieldsAbsentFromTheFileAtTheirZeroValue(t *testing.T) {
	neutralizeConfigEnv(t)
	location := writeConfigFile(t, `{"server_port": "8080"}`)

	config, err := LoadConfigLocation(location)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if config.LoggerLevel != "" {
		t.Errorf("LoggerLevel: expected the zero value, got %q", config.LoggerLevel)
	}
	if config.Debug != false {
		t.Errorf("Debug: expected the zero value, got %v", config.Debug)
	}
	if config.KafkaTopicConfigs != nil {
		t.Errorf("KafkaTopicConfigs: expected nil, got %v", config.KafkaTopicConfigs)
	}
	// The loader deliberately declares no defaults, so that replacing it did not
	// change which values a deployment ends up with.
	if config.KafkaPartitionNum != 0 || config.PostgresPort != 0 || config.JsTimeout != 0 {
		t.Errorf("expected no defaults to be injected, got %+v", config)
	}
}

func TestLoadConfigLocationIgnoresUnknownJsonKeys(t *testing.T) {
	neutralizeConfigEnv(t)
	location := writeConfigFile(t, `{"server_port": "8080", "this_key_does_not_exist": "whatever", "nested": {"a": 1}}`)

	config, err := LoadConfigLocation(location)
	if err != nil {
		t.Fatalf("expected unknown keys to be ignored, got error %v", err)
	}
	if config.ServerPort != "8080" {
		t.Errorf("ServerPort: expected \"8080\", got %q", config.ServerPort)
	}
}

func TestLoadConfigLocationReturnsAnErrorWhenTheFileIsMissing(t *testing.T) {
	neutralizeConfigEnv(t)
	location := filepath.Join(t.TempDir(), "does-not-exist.json")

	_, err := LoadConfigLocation(location)
	if err == nil {
		t.Fatal("expected an error for a missing config file, got nil")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected a not-exist error, got %v", err)
	}
}

func TestLoadConfigLocationReturnsAnErrorWhenTheJsonIsMalformed(t *testing.T) {
	neutralizeConfigEnv(t)
	location := writeConfigFile(t, `{"server_port": "8080",`)

	_, err := LoadConfigLocation(location)
	if err == nil {
		t.Fatal("expected an error for malformed json, got nil")
	}
}

func TestLoadConfigLocationReturnsAnErrorWhenAJsonValueHasTheWrongType(t *testing.T) {
	neutralizeConfigEnv(t)
	location := writeConfigFile(t, `{"server_port": 8080}`)

	_, err := LoadConfigLocation(location)
	if err == nil {
		t.Fatal("expected an error when a string field receives a json number, got nil")
	}
}

// config_hdl.Load skips empty paths, which would start the service on a
// zero-valued config. LoadConfigLocation keeps the old loader's behaviour of
// reporting the missing file instead.
func TestLoadConfigLocationReturnsAnErrorForAnEmptyLocation(t *testing.T) {
	neutralizeConfigEnv(t)

	_, err := LoadConfigLocation("")
	if err == nil {
		t.Fatal("expected an error for an empty config location, got nil")
	}
}

func TestLoadConfigLocationLetsTheEnvironmentOverrideTheFile(t *testing.T) {
	neutralizeConfigEnv(t)
	location := writeConfigFile(t, `{"server_port": "8080", "logger_level": "info", "debug": false}`)
	t.Setenv("SERVER_PORT", "9090")
	t.Setenv("DEBUG", "true")

	config, err := LoadConfigLocation(location)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if config.ServerPort != "9090" {
		t.Errorf("ServerPort: expected the environment value \"9090\", got %q", config.ServerPort)
	}
	if config.Debug != true {
		t.Errorf("Debug: expected the environment value true, got %v", config.Debug)
	}
	if config.LoggerLevel != "info" {
		t.Errorf("LoggerLevel: expected the file value \"info\" to survive, got %q", config.LoggerLevel)
	}
}

// TestLoadConfigLocationLoadsTheShippedConfigJson catches a config.json that no
// longer matches the struct -- a json type that stopped fitting its field is
// otherwise only found when the container starts.
func TestLoadConfigLocationLoadsTheShippedConfigJson(t *testing.T) {
	neutralizeConfigEnv(t)

	config, err := LoadConfigLocation("../../config.json")
	if err != nil {
		t.Fatalf("the shipped config.json must load: %v", err)
	}
	// js_timeout is a json number, i.e. nanoseconds, and must keep meaning that.
	if config.JsTimeout != 2*time.Second {
		t.Errorf("JsTimeout: expected 2s from js_timeout=2000000000, got %v", config.JsTimeout)
	}
	if config.MongoUrl.Value() != "mongodb://db" {
		t.Errorf("MongoUrl: expected \"mongodb://db\", got %q", config.MongoUrl.Value())
	}
	if len(config.KafkaTopicConfigs) == 0 {
		t.Error("KafkaTopicConfigs: expected the shipped topic configs, got none")
	}
	// state_flush_interval is a json number as well, for the same reason
	if config.StateFlushInterval != 5*time.Second {
		t.Errorf("StateFlushInterval: expected 5s from state_flush_interval=5000000000, got %v", config.StateFlushInterval)
	}
}

// ---------------------------------------------------------------------------
// the env_var tags
// ---------------------------------------------------------------------------

// The old loader derived the variable name from the field name. It does not any
// more, on purpose: a field without a tag is silently unconfigurable, which is
// exactly the failure this test exists to prevent. (It replaces
// TestFieldNameToEnvNameSplitsCamelCaseIntoScreamingSnakeCase, which tested the
// removed derivation helper.)
func TestEveryConfigFieldHasAnExplicitEnvVarTag(t *testing.T) {
	for fieldName, envName := range configEnvVarNames() {
		if envName == "" {
			t.Errorf("field %s has no env_var tag and can therefore not be configured", fieldName)
			continue
		}
		if envName != strings.ToUpper(envName) {
			t.Errorf("field %s: env_var %q is not upper case", fieldName, envName)
		}
		if strings.TrimSpace(envName) != envName {
			t.Errorf("field %s: env_var %q has surrounding whitespace", fieldName, envName)
		}
	}
}

// TestConfigFieldsMapToTheExpectedEnvironmentVariableNames is the migration
// safety net: it snapshots the complete environment variable surface of the
// service. Adding a field to Config without adding it here fails the test, and
// a renamed env_var tag fails it too. The expected names are the ones the
// previous, deriving loader produced.
func TestConfigFieldsMapToTheExpectedEnvironmentVariableNames(t *testing.T) {
	expected := map[string]string{
		"AsyncCompression":          "ASYNC_COMPRESSION",
		"AsyncFlushFrequency":       "ASYNC_FLUSH_FREQUENCY",
		"AsyncFlushMessages":        "ASYNC_FLUSH_MESSAGES",
		"AsyncPgThreadMax":          "ASYNC_PG_THREAD_MAX",
		"AuthClientId":              "AUTH_CLIENT_ID",
		"AuthClientSecret":          "AUTH_CLIENT_SECRET",
		"AuthEndpoint":              "AUTH_ENDPOINT",
		"AuthExpirationTimeBuffer":  "AUTH_EXPIRATION_TIME_BUFFER",
		"CharacteristicExpiration":  "CHARACTERISTIC_EXPIRATION",
		"Debug":                     "DEBUG",
		"DeviceExpiration":          "DEVICE_EXPIRATION",
		"DeviceLogTopic":            "DEVICE_LOG_TOPIC",
		"DeviceManagerUrl":          "DEVICE_MANAGER_URL",
		"DeviceRepoUrl":             "DEVICE_REPO_URL",
		"DeviceTypeExpiration":      "DEVICE_TYPE_EXPIRATION",
		"DeviceTypeTopic":           "DEVICE_TYPE_TOPIC",
		"EnvironmentCollectionName": "ENVIRONMENT_COLLECTION_NAME",
		"FatalKafkaError":           "FATAL_KAFKA_ERROR",
		"GatewayLogTopic":           "GATEWAY_LOG_TOPIC",
		"IotCacheMaxIdleConns":      "IOT_CACHE_MAX_IDLE_CONNS",
		"IotCacheTimeout":           "IOT_CACHE_TIMEOUT",
		"IotCacheUrls":              "IOT_CACHE_URLS",
		"JsTimeout":                 "JS_TIMEOUT",
		"JwtExpiration":             "JWT_EXPIRATION",
		"JwtIssuer":                 "JWT_ISSUER",
		"JwtPrivateKey":             "JWT_PRIVATE_KEY",
		"KafkaConsumerMaxBytes":     "KAFKA_CONSUMER_MAX_BYTES",
		"KafkaConsumerMaxWait":      "KAFKA_CONSUMER_MAX_WAIT",
		"KafkaConsumerMinBytes":     "KAFKA_CONSUMER_MIN_BYTES",
		"KafkaGroupName":            "KAFKA_GROUP_NAME",
		"KafkaPartitionNum":         "KAFKA_PARTITION_NUM",
		"KafkaReplicationFactor":    "KAFKA_REPLICATION_FACTOR",
		"KafkaResponseTopic":        "KAFKA_RESPONSE_TOPIC",
		"KafkaTopicConfigs":         "KAFKA_TOPIC_CONFIGS",
		"KafkaUrl":                  "KAFKA_URL",
		"LoggerHandler":             "LOGGER_HANDLER",
		"LoggerLevel":               "LOGGER_LEVEL",
		"MongoTable":                "MONGO_TABLE",
		"MongoUrl":                  "MONGO_URL",
		"NotificationUrl":           "NOTIFICATION_URL",
		"PermissionsV2Url":          "PERMISSIONS_V2_URL",
		"PostgresDb":                "POSTGRES_DB",
		"PostgresHost":              "POSTGRES_HOST",
		"PostgresPort":              "POSTGRES_PORT",
		"PostgresPw":                "POSTGRES_PW",
		"PostgresUser":              "POSTGRES_USER",
		"Protocol":                  "PROTOCOL",
		"ProtocolSegmentName":       "PROTOCOL_SEGMENT_NAME",
		"PublishToPostgres":         "PUBLISH_TO_POSTGRES",
		"ServerPort":                "SERVER_PORT",
		"StateCollectionName":       "STATE_COLLECTION_NAME",
		"StateFlushInterval":        "STATE_FLUSH_INTERVAL",
		"SyncCompression":           "SYNC_COMPRESSION",
		"TemplateCollectionName":    "TEMPLATE_COLLECTION_NAME",
		"TokenCacheExpiration":      "TOKEN_CACHE_EXPIRATION",
		"TokenCacheUrls":            "TOKEN_CACHE_URLS",
		"WorldCollectionName":       "WORLD_COLLECTION_NAME",
	}

	actual := configEnvVarNames()
	if !reflect.DeepEqual(actual, expected) {
		for fieldName, envName := range actual {
			if expected[fieldName] != envName {
				t.Errorf("field %s maps to %q, expected %q", fieldName, envName, expected[fieldName])
			}
		}
		for fieldName := range expected {
			if _, ok := actual[fieldName]; !ok {
				t.Errorf("field %s is gone from Config", fieldName)
			}
		}
	}
}

func TestNoTwoConfigFieldsShareAnEnvironmentVariableName(t *testing.T) {
	owner := map[string]string{}
	for fieldName, envName := range configEnvVarNames() {
		if previous, collides := owner[envName]; collides {
			t.Errorf("%s and %s both map to %s", previous, fieldName, envName)
		}
		owner[envName] = fieldName
	}
}

// TestTheVariablesSetByTheDeploymentArriveInTheConfig is the compatibility
// proof for the loader swap: these are the variables the production deployment
// exports. Each one has to land in its field, with the same meaning as before.
func TestTheVariablesSetByTheDeploymentArriveInTheConfig(t *testing.T) {
	neutralizeConfigEnv(t)
	location := writeConfigFile(t, `{"server_port": "8080"}`)

	t.Setenv("MONGO_URL", "mongodb://user:pw@mongo-0.mongo:27017/?replicaSet=rs0")
	t.Setenv("KAFKA_URL", "kafka.kafka:9092")
	t.Setenv("AUTH_ENDPOINT", "http://keycloak.keycloak:8080")
	t.Setenv("AUTH_CLIENT_ID", "moses")
	t.Setenv("AUTH_CLIENT_SECRET", "6d0a5f11-1f4f-4a1c-9c07-9f8f0f0f0f0f")
	t.Setenv("PERMISSIONS_V2_URL", "http://permv2.permissions:8080")
	t.Setenv("DEVICE_MANAGER_URL", "http://device-manager.device-management:8080")
	t.Setenv("DEVICE_REPO_URL", "http://device-repository.device-management:8080")
	t.Setenv("IOT_CACHE_URLS", "memcached-1:11211,memcached-2:11211")
	t.Setenv("TOKEN_CACHE_URLS", "memcached-1:11211,memcached-2:11211")
	t.Setenv("PROTOCOL_SEGMENT_NAME", "payload")
	t.Setenv("DEVICE_TYPE_EXPIRATION", "120")
	t.Setenv("DEVICE_EXPIRATION", "180")
	t.Setenv("PUBLISH_TO_POSTGRES", "true")
	t.Setenv("POSTGRES_PW", "postgrespw")
	t.Setenv("POSTGRES_HOST", "timescale.timescale")
	t.Setenv("KAFKA_CONSUMER_MAX_WAIT", "100ms")
	t.Setenv("NOTIFICATION_URL", "http://notifications.notifications:8080")

	config, err := LoadConfigLocation(location)
	if err != nil {
		t.Fatalf("expected the deployment variables to load, got %v", err)
	}

	if config.MongoUrl.Value() != "mongodb://user:pw@mongo-0.mongo:27017/?replicaSet=rs0" {
		t.Errorf("MongoUrl: got %q", config.MongoUrl.Value())
	}
	if config.KafkaUrl != "kafka.kafka:9092" {
		t.Errorf("KafkaUrl: got %q", config.KafkaUrl)
	}
	if config.AuthEndpoint != "http://keycloak.keycloak:8080" {
		t.Errorf("AuthEndpoint: got %q", config.AuthEndpoint)
	}
	if config.AuthClientId != "moses" {
		t.Errorf("AuthClientId: got %q", config.AuthClientId)
	}
	if config.AuthClientSecret.Value() != "6d0a5f11-1f4f-4a1c-9c07-9f8f0f0f0f0f" {
		t.Errorf("AuthClientSecret: got %q", config.AuthClientSecret.Value())
	}
	if config.PermissionsV2Url != "http://permv2.permissions:8080" {
		t.Errorf("PermissionsV2Url: got %q", config.PermissionsV2Url)
	}
	if config.DeviceManagerUrl != "http://device-manager.device-management:8080" {
		t.Errorf("DeviceManagerUrl: got %q", config.DeviceManagerUrl)
	}
	if config.DeviceRepoUrl != "http://device-repository.device-management:8080" {
		t.Errorf("DeviceRepoUrl: got %q", config.DeviceRepoUrl)
	}
	// the comma separated cache urls are plain strings, split downstream by
	// lib.StringToList -- they must NOT be parsed as json here
	if config.IotCacheUrls != "memcached-1:11211,memcached-2:11211" {
		t.Errorf("IotCacheUrls: got %q", config.IotCacheUrls)
	}
	if config.TokenCacheUrls != "memcached-1:11211,memcached-2:11211" {
		t.Errorf("TokenCacheUrls: got %q", config.TokenCacheUrls)
	}
	if config.ProtocolSegmentName != "payload" {
		t.Errorf("ProtocolSegmentName: got %q", config.ProtocolSegmentName)
	}
	if config.DeviceTypeExpiration != 120 {
		t.Errorf("DeviceTypeExpiration: got %v", config.DeviceTypeExpiration)
	}
	if config.DeviceExpiration != 180 {
		t.Errorf("DeviceExpiration: got %v", config.DeviceExpiration)
	}
	if config.PublishToPostgres != true {
		t.Errorf("PublishToPostgres: got %v", config.PublishToPostgres)
	}
	if config.PostgresPw.Value() != "postgrespw" {
		t.Errorf("PostgresPw: got %q", config.PostgresPw.Value())
	}
	if config.PostgresHost != "timescale.timescale" {
		t.Errorf("PostgresHost: got %q", config.PostgresHost)
	}
	if config.KafkaConsumerMaxWait != "100ms" {
		t.Errorf("KafkaConsumerMaxWait: got %q", config.KafkaConsumerMaxWait)
	}
	if config.NotificationUrl != "http://notifications.notifications:8080" {
		t.Errorf("NotificationUrl: got %q", config.NotificationUrl)
	}
}

// ---------------------------------------------------------------------------
// the environment, per reflect.Kind
//
// Config has no plain string-slice or string-map field, so the kind matrix is
// exercised on local structs that carry the same env_var tags.
// ---------------------------------------------------------------------------

type kindProbe struct {
	StringField  string            `env_var:"STRING_FIELD"`
	Int64Field   int64             `env_var:"INT64_FIELD"`
	IntField     int               `env_var:"INT_FIELD"`
	Float64Field float64           `env_var:"FLOAT64_FIELD"`
	BoolField    bool              `env_var:"BOOL_FIELD"`
	SliceField   []string          `env_var:"SLICE_FIELD"`
	MapField     map[string]string `env_var:"MAP_FIELD"`
}

type intMapProbe struct {
	MapField map[string]int `env_var:"INT_MAP_FIELD"`
}

func TestLoadOverridesAStringField(t *testing.T) {
	neutralizeProbeEnv(t)
	t.Setenv("STRING_FIELD", " spaces are kept ")
	probe := kindProbe{StringField: "from-file"}

	if err := loadProbe(t, &probe); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if probe.StringField != " spaces are kept " {
		t.Errorf("expected the raw environment value, got %q", probe.StringField)
	}
}

func TestLoadOverridesAnInt64Field(t *testing.T) {
	neutralizeProbeEnv(t)
	t.Setenv("INT64_FIELD", "-9223372036854775808")
	probe := kindProbe{Int64Field: 42}

	if err := loadProbe(t, &probe); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if probe.Int64Field != -9223372036854775808 {
		t.Errorf("expected math.MinInt64, got %v", probe.Int64Field)
	}
}

// WAS: TestHandleEnvironmentVarsIgnoresAnIntFieldBecauseOnlyInt64IsHandled.
// The old loader only handled reflect.Int64, so every plain `int` field was
// silently unconfigurable. It is configurable now.
func TestLoadOverridesAnIntField(t *testing.T) {
	neutralizeProbeEnv(t)
	t.Setenv("INT_FIELD", "5")
	probe := kindProbe{IntField: 42}

	if err := loadProbe(t, &probe); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if probe.IntField != 5 {
		t.Errorf("expected the int field to be set to 5, got %v", probe.IntField)
	}
}

// WAS: TestHandleEnvironmentVarsIgnoresAFloat64FieldBecauseTheKindIsNotHandled.
func TestLoadOverridesAFloat64Field(t *testing.T) {
	neutralizeProbeEnv(t)
	t.Setenv("FLOAT64_FIELD", "1.5")
	probe := kindProbe{Float64Field: 42}

	if err := loadProbe(t, &probe); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if probe.Float64Field != 1.5 {
		t.Errorf("expected the float field to be set to 1.5, got %v", probe.Float64Field)
	}
}

func TestLoadOverridesABoolField(t *testing.T) {
	neutralizeProbeEnv(t)
	t.Setenv("BOOL_FIELD", "TRUE")
	probe := kindProbe{}

	if err := loadProbe(t, &probe); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if probe.BoolField != true {
		t.Errorf("expected strconv.ParseBool to accept \"TRUE\", got %v", probe.BoolField)
	}
}

// WAS: TestHandleEnvironmentVarsSplitsASliceFieldOnCommasAndTrimsWhitespace.
// BEHAVIOUR CHANGE: slices come from json now, not from a comma separated list.
// Config has no slice field, so no deployment variable is affected.
func TestLoadParsesASliceFieldFromJson(t *testing.T) {
	neutralizeProbeEnv(t)
	t.Setenv("SLICE_FIELD", `["a", "b", "c"]`)
	probe := kindProbe{SliceField: []string{"replaced"}}

	if err := loadProbe(t, &probe); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	expected := []string{"a", "b", "c"}
	if !reflect.DeepEqual(probe.SliceField, expected) {
		t.Errorf("expected %v, got %v", expected, probe.SliceField)
	}
}

func TestLoadRejectsACommaSeparatedListForASliceField(t *testing.T) {
	neutralizeProbeEnv(t)
	t.Setenv("SLICE_FIELD", " a , b,c ")
	probe := kindProbe{SliceField: []string{"keep"}}

	err := loadProbe(t, &probe)
	if err == nil {
		t.Fatal("expected an error for a non-json slice value, got nil")
	}
	if !reflect.DeepEqual(probe.SliceField, []string{"keep"}) {
		t.Errorf("expected the field to be untouched on error, got %v", probe.SliceField)
	}
}

// WAS: TestHandleEnvironmentVarsParsesAMapFieldFromCommaSeparatedKeyColonValuePairs.
// BEHAVIOUR CHANGE: maps come from json now. The only map in Config is
// KafkaTopicConfigs, which could not be set from the environment at all before
// (it panicked), so no working deployment variable changes meaning.
func TestLoadParsesAMapFieldFromJson(t *testing.T) {
	neutralizeProbeEnv(t)
	t.Setenv("MAP_FIELD", `{"a": "1", "b": "2"}`)
	probe := kindProbe{MapField: map[string]string{"replaced": "yes"}}

	if err := loadProbe(t, &probe); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	expected := map[string]string{"a": "1", "b": "2"}
	if !reflect.DeepEqual(probe.MapField, expected) {
		t.Errorf("expected %v, got %v", expected, probe.MapField)
	}
}

// WAS: TestHandleEnvironmentVarsTruncatesAMapValueAtItsSecondColon, which
// asserted that "endpoint:localhost:9092" silently became {"endpoint":
// "localhost"}. A colon in the value survives now.
func TestLoadKeepsColonsInsideAMapValue(t *testing.T) {
	neutralizeProbeEnv(t)
	t.Setenv("MAP_FIELD", `{"endpoint": "localhost:9092"}`)
	probe := kindProbe{}

	if err := loadProbe(t, &probe); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	expected := map[string]string{"endpoint": "localhost:9092"}
	if !reflect.DeepEqual(probe.MapField, expected) {
		t.Errorf("expected %v, got %v", expected, probe.MapField)
	}
}

// WAS: TestHandleEnvironmentVarsPanicsWhenAMapEntryHasNoColon. The old loader
// indexed keyVal[1] out of range and crashed the process during start-up.
func TestLoadReportsAnErrorInsteadOfPanickingForAMalformedMapValue(t *testing.T) {
	neutralizeProbeEnv(t)
	t.Setenv("MAP_FIELD", "no-colon-here")
	probe := kindProbe{MapField: map[string]string{"keep": "yes"}}

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("expected no panic, got %v", recovered)
		}
	}()
	err := loadProbe(t, &probe)
	if err == nil {
		t.Fatal("expected an error for a malformed map value, got nil")
	}
	if !reflect.DeepEqual(probe.MapField, map[string]string{"keep": "yes"}) {
		t.Errorf("expected the field to be untouched on error, got %v", probe.MapField)
	}
}

// WAS: TestHandleEnvironmentVarsPanicsWhenTheMapValueTypeIsNotString. The old
// loader always built a map[string]string and handed it to reflect.Value.Set,
// which panicked for any other map type.
func TestLoadFillsAMapWithANonStringValueTypeFromJson(t *testing.T) {
	neutralizeProbeEnv(t)
	t.Setenv("INT_MAP_FIELD", `{"a": 1}`)
	probe := intMapProbe{}

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("expected no panic, got %v", recovered)
		}
	}()
	if err := loadProbe(t, &probe); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	expected := map[string]int{"a": 1}
	if !reflect.DeepEqual(probe.MapField, expected) {
		t.Errorf("expected %v, got %v", expected, probe.MapField)
	}
}

func TestLoadLeavesFieldsUntouchedWhenNoVariableIsSet(t *testing.T) {
	neutralizeProbeEnv(t)
	probe := kindProbe{
		StringField:  "keep",
		Int64Field:   7,
		IntField:     8,
		Float64Field: 9.5,
		BoolField:    true,
		SliceField:   []string{"keep"},
		MapField:     map[string]string{"keep": "yes"},
	}
	expected := probe

	if err := loadProbe(t, &probe); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !reflect.DeepEqual(probe, expected) {
		t.Errorf("expected %+v to be unchanged, got %+v", expected, probe)
	}
}

// WAS: TestHandleEnvironmentVarsCannotClearAValueWithAnEmptyVariable. The old
// loader skipped on `envValue != ""`, so an operator had no way to switch a
// setting back off from the environment. The new loader uses os.LookupEnv and
// distinguishes "absent" from "empty".
//
// BEHAVIOUR CHANGE, DEPLOYMENT VISIBLE: exporting a variable as the empty
// string now overrides the config file value instead of being ignored.
func TestLoadClearsAStringFieldWithAnEmptyVariable(t *testing.T) {
	neutralizeProbeEnv(t)
	t.Setenv("STRING_FIELD", "")
	probe := kindProbe{StringField: "from-file"}

	if err := loadProbe(t, &probe); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if probe.StringField != "" {
		t.Errorf("expected the empty variable to clear the field, got %q", probe.StringField)
	}
}

// The flip side of the same change: for a numeric or boolean field the empty
// string is not a value, so it is a hard error at start-up rather than a silent
// 0 / false. Pinned because a deployment that exports one of these as "" used
// to start and now does not.
func TestLoadReportsAnErrorForAnEmptyNumericOrBooleanVariable(t *testing.T) {
	for _, envName := range []string{"INT64_FIELD", "INT_FIELD", "FLOAT64_FIELD", "BOOL_FIELD"} {
		t.Run(envName, func(t *testing.T) {
			neutralizeProbeEnv(t)
			t.Setenv(envName, "")
			probe := kindProbe{Int64Field: 7, IntField: 8, Float64Field: 9.5, BoolField: true}

			if err := loadProbe(t, &probe); err == nil {
				t.Fatalf("%s=\"\": expected an error, got nil", envName)
			}
		})
	}
}

// WAS: TestHandleEnvironmentVarsSilentlyZeroesAnInt64FieldForAnUnparsableValue.
// The old loader discarded the strconv error, so a typo replaced the configured
// value with 0 -- for AsyncPgThreadMax, KafkaConsumerMaxBytes or
// TokenCacheExpiration a production-affecting difference.
func TestLoadReportsAnErrorForAnUnparsableInt64AndLeavesTheValueUntouched(t *testing.T) {
	neutralizeProbeEnv(t)
	t.Setenv("INT64_FIELD", "12x")
	probe := kindProbe{Int64Field: 42}

	err := loadProbe(t, &probe)
	if err == nil {
		t.Fatal("expected an error for an unparsable int64, got nil")
	}
	if probe.Int64Field != 42 {
		t.Errorf("expected the configured value to survive the error, got %v", probe.Int64Field)
	}
}

// WAS: TestHandleEnvironmentVarsSilentlyFalsifiesABoolFieldForAnUnparsableValue.
// The old loader turned FATAL_KAFKA_ERROR=yes into false, i.e. it silently
// switched the fail-fast safety net OFF. Both the probe and the real field are
// checked, because this one was reachable from a deployment.
func TestLoadReportsAnErrorForAnUnparsableBool(t *testing.T) {
	neutralizeProbeEnv(t)
	t.Setenv("BOOL_FIELD", "yes")
	probe := kindProbe{BoolField: true}

	err := loadProbe(t, &probe)
	if err == nil {
		t.Fatal("expected an error for an unparsable bool, got nil")
	}
	if probe.BoolField != true {
		t.Errorf("expected the configured value to survive the error, got %v", probe.BoolField)
	}

	neutralizeConfigEnv(t)
	location := writeConfigFile(t, `{"fatal_kafka_error": true}`)
	t.Setenv("FATAL_KAFKA_ERROR", "yes")
	if _, err := LoadConfigLocation(location); err == nil {
		t.Fatal("expected FATAL_KAFKA_ERROR=yes to fail the config load, got nil")
	}
}

// ---------------------------------------------------------------------------
// the real Config, for the traps above
// ---------------------------------------------------------------------------

// WAS: TestHandleEnvironmentVarsTreatsJsTimeoutAsRawNanoseconds.
// BEHAVIOUR CHANGE, DEPLOYMENT VISIBLE: JS_TIMEOUT is parsed by
// time.ParseDuration now, so it takes "2s" and rejects a raw nanosecond count.
// The json key js_timeout keeps meaning nanoseconds (see
// TestLoadConfigLocationLoadsTheShippedConfigJson).
func TestJsTimeoutTakesADurationStringFromTheEnvironment(t *testing.T) {
	neutralizeConfigEnv(t)
	location := writeConfigFile(t, `{"js_timeout": 60000000000}`)

	t.Setenv("JS_TIMEOUT", "5s")
	durationString, err := LoadConfigLocation(location)
	if err != nil {
		t.Fatalf("JS_TIMEOUT=5s: expected no error, got %v", err)
	}
	if durationString.JsTimeout != 5*time.Second {
		t.Errorf("JS_TIMEOUT=5s: expected 5s, got %v", durationString.JsTimeout)
	}

	t.Setenv("JS_TIMEOUT", "5000000000")
	if _, err := LoadConfigLocation(location); err == nil {
		t.Fatal("JS_TIMEOUT=5000000000: expected an error for the missing unit, got nil")
	}
}

// WAS: TestLoadConfigLocationPanicsWhenKafkaTopicConfigsIsSetInTheEnvironment.
// There was no value that worked: the old loader built a map[string]string and
// reflect.Set panicked on the map[string][]kafka.ConfigEntry field, so the
// service crashed at start-up for ANY value of KAFKA_TOPIC_CONFIGS.
func TestKafkaTopicConfigsIsConfiguredAsJsonFromTheEnvironment(t *testing.T) {
	neutralizeConfigEnv(t)
	location := writeConfigFile(t, `{"kafka_topic_configs": {"from-file": [{"ConfigName": "retention.ms", "ConfigValue": "1"}]}}`)
	t.Setenv("KAFKA_TOPIC_CONFIGS", `{"topic-a": [{"ConfigName": "retention.ms", "ConfigValue": "-1"}, {"ConfigName": "cleanup.policy", "ConfigValue": "compact"}]}`)

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("expected no panic, got %v", recovered)
		}
	}()
	config, err := LoadConfigLocation(location)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	expected := map[string][]kafka.ConfigEntry{
		"topic-a": {
			{ConfigName: "retention.ms", ConfigValue: "-1"},
			{ConfigName: "cleanup.policy", ConfigValue: "compact"},
		},
	}
	if !reflect.DeepEqual(config.KafkaTopicConfigs, expected) {
		t.Errorf("expected %v, got %v", expected, config.KafkaTopicConfigs)
	}
}

func TestKafkaTopicConfigsReportsAnErrorForAMalformedValueWithoutPanicking(t *testing.T) {
	for _, value := range []string{"topic-a:retention.ms", "{", `{"topic-a": "not-a-list"}`} {
		t.Run(value, func(t *testing.T) {
			neutralizeConfigEnv(t)
			location := writeConfigFile(t, `{"server_port": "8080"}`)
			t.Setenv("KAFKA_TOPIC_CONFIGS", value)

			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("expected no panic, got %v", recovered)
				}
			}()
			if _, err := LoadConfigLocation(location); err == nil {
				t.Fatalf("KAFKA_TOPIC_CONFIGS=%q: expected an error, got nil", value)
			}
		})
	}
}

// WAS: TestHandleEnvironmentVarsIgnoresTheIntAndFloatFieldsOfConfig. These four
// fields were silently unconfigurable from the environment.
func TestTheIntAndFloatFieldsOfConfigAreConfigurableFromTheEnvironment(t *testing.T) {
	neutralizeConfigEnv(t)
	location := writeConfigFile(t, `{
		"kafka_partition_num": 1,
		"kafka_replication_factor": 1,
		"postgres_port": 5432,
		"auth_expiration_time_buffer": 0.5
	}`)
	t.Setenv("KAFKA_PARTITION_NUM", "3")
	t.Setenv("KAFKA_REPLICATION_FACTOR", "2")
	t.Setenv("POSTGRES_PORT", "5433")
	t.Setenv("AUTH_EXPIRATION_TIME_BUFFER", "1.5")

	config, err := LoadConfigLocation(location)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if config.KafkaPartitionNum != 3 {
		t.Errorf("KafkaPartitionNum: expected 3, got %v", config.KafkaPartitionNum)
	}
	if config.KafkaReplicationFactor != 2 {
		t.Errorf("KafkaReplicationFactor: expected 2, got %v", config.KafkaReplicationFactor)
	}
	if config.PostgresPort != 5433 {
		t.Errorf("PostgresPort: expected 5433, got %v", config.PostgresPort)
	}
	if config.AuthExpirationTimeBuffer != 1.5 {
		t.Errorf("AuthExpirationTimeBuffer: expected 1.5, got %v", config.AuthExpirationTimeBuffer)
	}
}

// ---------------------------------------------------------------------------
// secrets
// ---------------------------------------------------------------------------

type secretProbe struct {
	PlainField  string                 `json:"plain_field" env_var:"PLAIN_FIELD"`
	SecretField sb_config_types.Secret `json:"secret_field" env_var:"SECRET_FIELD"`
}

func TestSecretFieldsArePopulatedFromTheEnvironment(t *testing.T) {
	neutralizeProbeEnv(t)
	t.Setenv("PLAIN_FIELD", "plain-value")
	t.Setenv("SECRET_FIELD", "s3cr3t-value")
	probe := secretProbe{}

	if err := loadProbe(t, &probe); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if probe.SecretField.Value() != "s3cr3t-value" {
		t.Errorf("expected the secret field to be populated, got %q", probe.SecretField.Value())
	}
	if probe.PlainField != "plain-value" {
		t.Errorf("expected the plain field to be populated, got %q", probe.PlainField)
	}
}

// The `config:"secret"` tag of the old loader only suppressed its own "use
// environment variable: ..." line; anything else that formatted the Config
// printed the credential in the clear. types.Secret masks at the value, so a
// log.Printf("%+v", config), an error message and a json.Marshal are all safe.
func TestSecretFieldsAreMaskedWhenFormattedOrMarshalled(t *testing.T) {
	neutralizeProbeEnv(t)
	t.Setenv("SECRET_FIELD", "s3cr3t-value")
	probe := secretProbe{}
	if err := loadProbe(t, &probe); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	marshalled, err := json.Marshal(probe)
	if err != nil {
		t.Fatalf("unable to marshal: %v", err)
	}
	renderings := []string{
		fmt.Sprintf("%v", probe),
		fmt.Sprintf("%+v", probe),
		fmt.Sprintf("%v", probe.SecretField),
		fmt.Sprintf("%s", probe.SecretField),
		probe.SecretField.String(),
		string(marshalled),
	}
	for _, rendering := range renderings {
		if strings.Contains(rendering, "s3cr3t-value") {
			t.Errorf("secret leaked into %q", rendering)
		}
	}
	// masking must not damage the value itself
	if probe.SecretField.Value() != "s3cr3t-value" {
		t.Errorf("expected Value() to return the plaintext, got %q", probe.SecretField.Value())
	}
}

// WAS: TestSecretTaggedFieldsAreNotPrintedByTheLoader and
// TestNonSecretFieldsArePrintedByTheLoaderWithTheirValue.
// BEHAVIOUR CHANGE, OPERATOR VISIBLE: the old loader announced every override
// on stdout ("use environment variable: PLAIN_FIELD = plain-value"). The new
// one is silent, for secrets and non-secrets alike. Nothing can leak, but the
// operator also loses the confirmation that a variable took effect.
func TestTheLoaderDoesNotPrintOverridesToStdout(t *testing.T) {
	neutralizeConfigEnv(t)
	location := writeConfigFile(t, `{"server_port": "8080"}`)
	t.Setenv("SERVER_PORT", "9090")
	t.Setenv("MONGO_URL", "mongodb://user:s3cr3t-value@localhost:27017")
	// deliberately not shaped like a real PEM header. The secret scanner is
	// meant to catch that shape, and allowlisting this file to keep a fixture
	// would also hide a real key added here later.
	t.Setenv("JWT_PRIVATE_KEY", "jwt-private-key-fixture-value")

	output := captureStdout(t, func() {
		if _, err := LoadConfigLocation(location); err != nil {
			t.Errorf("expected no error, got %v", err)
		}
	})

	if strings.Contains(output, "s3cr3t-value") || strings.Contains(output, "jwt-private-key-fixture-value") {
		t.Errorf("a credential leaked to stdout: %q", output)
	}
	if output != "" {
		t.Errorf("expected the loader to print nothing, got %q", output)
	}
}

// WAS: TestTheSecretTaggedFieldsOfConfigAreTheExpectedOnes, which listed the
// fields carrying `config:"secret"`. The tag is gone; the type is the marker
// now. Changes against that list:
//   - JwtPrivateKey is a Secret now. It is a private key and was previously
//     untagged, i.e. printed in the clear.
//   - AuthClientId is NOT a Secret. An OAuth2 client id is a public identifier
//     (RFC 6749 section 2.2), not a credential, and keeping it readable helps
//     diagnose an auth misconfiguration.
func TestTheSecretTypedFieldsOfConfigAreTheExpectedOnes(t *testing.T) {
	secretType := reflect.TypeOf(sb_config_types.Secret(""))
	configType := reflect.TypeOf(Config{})
	actual := []string{}
	for index := 0; index < configType.NumField(); index++ {
		if configType.Field(index).Type == secretType {
			actual = append(actual, configType.Field(index).Name)
		}
	}
	expected := []string{"MongoUrl", "AuthClientSecret", "JwtPrivateKey", "PostgresUser", "PostgresPw"}
	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("expected secret fields %v, got %v", expected, actual)
	}
}

// WAS: TestJwtPrivateKeyIsPrintedInTheClearBecauseItIsNotTaggedSecret, which
// documented a real leak. The field is a Secret now, so the assertion is
// inverted on purpose.
func TestJwtPrivateKeyIsMaskedBecauseItIsASecret(t *testing.T) {
	neutralizeConfigEnv(t)
	location := writeConfigFile(t, `{"server_port": "8080"}`)
	// same reason as above: the value only has to be recognisable in output,
	// it does not have to look like a key
	const privateKey = "jwt-private-key-fixture-value"
	t.Setenv("JWT_PRIVATE_KEY", privateKey)

	config, err := LoadConfigLocation(location)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if config.JwtPrivateKey.Value() != privateKey {
		t.Errorf("expected the private key to be loaded, got %q", config.JwtPrivateKey.Value())
	}
	if rendered := fmt.Sprintf("%+v", config); strings.Contains(rendered, privateKey) {
		t.Errorf("the private key leaked into %q", rendered)
	}
}
