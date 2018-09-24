package moses

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

	router.GET("/worlds", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		b, err := json.Marshal(state.Worlds)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(w, err.Error(), 500)
		} else {
			fmt.Fprint(w, string(b))
		}
	})

	router.GET("/world/:worldid", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		world, ok := state.Worlds[ps.ByName("id")]
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

	router.GET("/world/:worldid/rooms", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
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

	router.GET("/world/:worldid/room/:roomid", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
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

	router.GET("/world/:worldid/room/:roomid/devices", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
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

	router.GET("/world/:worldid/room/:roomid/device/:deviceid", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
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

	router.PUT("/world", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
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

	router.PUT("/world/:worldid/room", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		room := Room{}
		err := json.NewDecoder(r.Body).Decode(&room)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(w, err.Error(), 400)
			return
		}
		world, ok := state.Worlds[ps.ByName("worldid")]
		if !ok {
			log.Println("404")
			http.Error(w, "world not found", 404)
			return
		}
		err = state.UpdateRoom(world.Id, room)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprint(w, "ok")
	})

	router.PUT("/world/:worldid/room/:roomid/device", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		device := Device{}
		err := json.NewDecoder(r.Body).Decode(&device)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(w, err.Error(), 400)
			return
		}
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
