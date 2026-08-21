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

import (
	sb_config_hdl "github.com/SENERGY-Platform/go-service-base/config-hdl"
	sb_config_env_parser "github.com/SENERGY-Platform/go-service-base/config-hdl/env_parser"
	sb_config_types "github.com/SENERGY-Platform/go-service-base/config-hdl/types"
)

// envTypeParsers registers the parsers for the field types the generic
// kind-based fallback of go-env-loader cannot handle.
//
// DurationEnvTypeParser is not optional: without it a time.Duration field falls
// through to the int64 kind parser, which hands a plain int64 to
// reflect.Value.Set and panics with "int64 is not assignable to
// time.Duration". With it, JS_TIMEOUT takes a duration string ("2s").
var envTypeParsers = []sb_config_hdl.EnvTypeParser{
	sb_config_types.SecretEnvTypeParser,
	sb_config_env_parser.DurationEnvTypeParser,
}
