/*
 * Copyright 2018 SENERGY Team
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

type CreateWorldRequest struct {
	Name   string                 `json:"name"`
	States map[string]interface{} `json:"states"`
}

type UpdateWorldRequest struct {
	Id     string                 `json:"id"`
	Name   string                 `json:"name"`
	States map[string]interface{} `json:"states"`
}

type RoomResponse struct {
	World string `json:"world"`
	Room  Room   `json:"room"`
}

type UpdateRoomRequest struct {
	Id     string                 `json:"id"`
	Name   string                 `json:"name"`
	States map[string]interface{} `json:"states"`
}

type CreateRoomRequest struct {
	World  string                 `json:"world"`
	Name   string                 `json:"name"`
	States map[string]interface{} `json:"states"`
}
