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

package lib

import (
	"encoding/json"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/util"
	platform_connector_lib "github.com/SENERGY-Platform/platform-connector-lib"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
)

// commandHandler is the shape both runtimes offer: the new runtime's
// HandleCommand additionally says whether it was responsible, the legacy one
// always tries.
type commandHandler func(externalDeviceRef string, externalServiceRef string, cmdMsg interface{}, responder func(respMsg interface{}))

// asyncCommandHandler wraps a command handler into what the connector expects.
//
// This used to live in state.StateRepo.Start(), where it was re-registered on
// every legacy api call. It moved here unchanged, because the connector takes
// exactly one handler and the handler now has to try the new runtime first: the
// envelope handling below is the part both runtimes share, and duplicating it
// would be the way to make the two disagree about the protocol segment.
//
// The decode is deliberately forgiving in the same way as before: a segment that
// is not json is passed on as the raw string rather than dropped, because a
// legacy script may well expect exactly that string.
func asyncCommandHandler(config config.Config, connector *platform_connector_lib.Connector, handle commandHandler) platform_connector_lib.AsyncCommandHandler {
	return func(commandRequest model.ProtocolMsg, requestMsg platform_connector_lib.CommandRequestMsg, t time.Time) (err error) {
		msg := map[string]interface{}{}
		for key, value := range requestMsg {
			var msgPart interface{}
			err = json.Unmarshal([]byte(value), &msgPart)
			if err != nil {
				msgPart = value
			}
			msg[key] = msgPart
		}
		handle(commandRequest.Metadata.Device.Id, commandRequest.Metadata.Service.Id, msg[config.ProtocolSegmentName], func(respMsg interface{}) {
			response := platform_connector_lib.CommandResponseMsg{}
			msgStr, err := json.Marshal(respMsg)
			if err != nil {
				util.Logger.Warn("unable to marshal command response", attributes.ErrorKey, err)
				return
			}
			response[config.ProtocolSegmentName] = string(msgStr)
			err = connector.HandleCommandResponse(commandRequest, response, platform_connector_lib.Sync)
			if err != nil {
				util.Logger.Error("unable to send command response", attributes.ErrorKey, err)
				return
			}
		})
		//the error return is deliberately always nil, as it was before: a script
		//that cannot handle a command is logged, not retried by the connector
		return nil
	}
}
