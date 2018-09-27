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

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/julienschmidt/httprouter"
)

func StartApi(config Config, staterepo *StateRepo) {
	httpHandler := getRoutes(config, staterepo)
	logger := Logger(httpHandler, config.LogLevel)
	log.Println(http.ListenAndServe(":"+config.ServerPort, logger))
}

func getRoutes(config Config, state *StateRepo) *httprouter.Router {
	router := httprouter.New()

	router.GET("/dev/worlds", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		state.mux.RLock()
		defer state.mux.RUnlock()
		b, err := json.Marshal(state.Worlds)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(w, err.Error(), 500)
		} else {
			fmt.Fprint(w, string(b))
		}
	})

	router.GET("/dev/world/:worldid", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		state.mux.RLock()
		defer state.mux.RUnlock()
		world, ok := state.Worlds[ps.ByName("worldid")]
		if !ok {
			log.Println("404")
			http.Error(w, "not found", 404)
			return
		}
		b, err := json.Marshal(world)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(w, err.Error(), 500)
		} else {
			fmt.Fprint(w, string(b))
		}
	})

	router.GET("/dev/world/:worldid/rooms", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		state.mux.RLock()
		defer state.mux.RUnlock()
		world, ok := state.Worlds[ps.ByName("worldid")]
		if !ok {
			log.Println("404")
			http.Error(w, "world not found", 404)
			return
		}
		b, err := json.Marshal(world.Rooms)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(w, err.Error(), 500)
		} else {
			fmt.Fprint(w, string(b))
		}
	})

	router.GET("/dev/world/:worldid/room/:roomid", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		state.mux.RLock()
		defer state.mux.RUnlock()
		world, ok := state.Worlds[ps.ByName("worldid")]
		if !ok {
			log.Println("404")
			http.Error(w, "world not found", 404)
			return
		}
		room, ok := world.Rooms[ps.ByName("roomid")]
		if !ok {
			log.Println("404")
			http.Error(w, "room not found", 404)
			return
		}
		b, err := json.Marshal(room)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(w, err.Error(), 500)
		} else {
			fmt.Fprint(w, string(b))
		}
	})

	router.GET("/dev/world/:worldid/room/:roomid/devices", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		state.mux.RLock()
		defer state.mux.RUnlock()
		world, ok := state.Worlds[ps.ByName("worldid")]
		if !ok {
			log.Println("404")
			http.Error(w, "world not found", 404)
			return
		}
		room, ok := world.Rooms[ps.ByName("roomid")]
		if !ok {
			log.Println("404")
			http.Error(w, "room not found", 404)
			return
		}
		b, err := json.Marshal(room.Devices)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(w, err.Error(), 500)
		} else {
			fmt.Fprint(w, string(b))
		}
	})

	router.GET("/dev/world/:worldid/room/:roomid/device/:deviceid", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		state.mux.RLock()
		defer state.mux.RUnlock()
		world, ok := state.Worlds[ps.ByName("worldid")]
		if !ok {
			log.Println("404")
			http.Error(w, "world not found", 404)
			return
		}
		room, ok := world.Rooms[ps.ByName("roomid")]
		if !ok {
			log.Println("404")
			http.Error(w, "room not found", 404)
			return
		}
		device, ok := room.Devices[ps.ByName("deviceid")]
		if !ok {
			log.Println("404")
			http.Error(w, "device not found", 404)
			return
		}
		b, err := json.Marshal(device)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(w, err.Error(), 500)
		} else {
			fmt.Fprint(w, string(b))
		}
	})

	router.PUT("/dev/world", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		world := World{}
		err := json.NewDecoder(r.Body).Decode(&world)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(w, err.Error(), 400)
			return
		}
		err = state.UpdateWorld(world)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprint(w, "ok")
	})

	router.PUT("/dev/world/:worldid/room", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		room := Room{}
		err := json.NewDecoder(r.Body).Decode(&room)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(w, err.Error(), 400)
			return
		}
		state.mux.RLock()
		world, ok := state.Worlds[ps.ByName("worldid")]
		if !ok {
			log.Println("404")
			http.Error(w, "world not found", 404)
			return
		}
		state.mux.RUnlock()
		err = state.UpdateRoom(world.Id, room)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprint(w, "ok")
	})

	router.PUT("/dev/world/:worldid/room/:roomid/device", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		device := Device{}
		err := json.NewDecoder(r.Body).Decode(&device)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(w, err.Error(), 400)
			return
		}
		state.mux.RLock()
		world, ok := state.Worlds[ps.ByName("worldid")]
		if !ok {
			log.Println("404")
			http.Error(w, "world not found", 404)
			return
		}
		room, ok := world.Rooms[ps.ByName("roomid")]
		if !ok {
			log.Println("404")
			http.Error(w, "room not found", 404)
			return
		}
		state.mux.RUnlock()
		err = state.UpdateDevice(world.Id, room.Id, device)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprint(w, "ok")
	})

	return router
}
