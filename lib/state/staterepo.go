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

package state

import (
	"encoding/json"
	"errors"
	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/crashbrake"
	"github.com/SENERGY-Platform/moses/lib/platformhttp"
	"github.com/SENERGY-Platform/moses/lib/util"
	platform_connector_lib "github.com/SENERGY-Platform/platform-connector-lib"
	"github.com/SENERGY-Platform/platform-connector-lib/connectionlog"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
	"sync"
	"time"

	"github.com/google/uuid"
)

type StateRepo struct {
	Worlds                 map[string]*World
	Persistence            PersistenceInterface
	Connector              *platform_connector_lib.Connector
	Config                 config.Config
	changeRoutineIndex     map[string]ChangeRoutineIndexElement
	externalRefDeviceIndex map[string]*Device
	serviceDeviceIndex     map[string]*Device
	deviceRoomIndex        map[string]*Room
	deviceWorldIndex       map[string]*World
	roomWorldIndex         map[string]*World
	changeRoutinesTickers  []*time.Ticker
	stopChannels           []chan bool
	mux                    sync.RWMutex
	MosesProtocolId        string
	StateLogger            connectionlog.Logger

	// PlatformClients call the device-manager; lib.New shares them with the catalog.
	// An unset read client is built from Config.PlatformHttpTimeout.
	PlatformClients platformhttp.Clients
	// externalWriteTimeout bounds one device-manager write; zero is platformhttp.WriteTimeout.
	externalWriteTimeout time.Duration

	// Brake records, per world, when a script run is in flight, so a fatal crash
	// is quarantined on the next boot instead of looping. nil disables it. lib.New
	// sets it.
	Brake *crashbrake.Brake

	// SkipWorldIds names the worlds this runtime must not start, because they
	// exist as an environment and are run by lib/runtime instead. Both runtimes
	// publish under the same platform device and service ids, so starting a
	// migrated world here would send every value twice.
	//
	// It is a plain set of ids rather than a reference to the environment store
	// on purpose: this package is being replaced, and it must not grow a
	// dependency on the package replacing it. lib.New fills it in.
	SkipWorldIds map[string]bool

	// shutDown is set under mux by Shutdown; from then on no update starts the routines again.
	shutDown bool
}

// Update for HTTP-DEV-API
// Stops all change routines and redeploys new world
// requests a mutex lock on the state repo
func (this *StateRepo) DevUpdateWorld(worldMsg WorldMsg) (err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	world, err := worldMsg.ToModel()
	if err != nil {
		util.Logger.Error("unable to convert world message to model", attributes.ErrorKey, err)
		return err
	}
	if this.Worlds == nil {
		this.Worlds = map[string]*World{}
	}
	if world.Id == "" {
		uid, err := uuid.NewRandom()
		if err != nil {
			util.Logger.Error("unable to generate world id", attributes.ErrorKey, err)
			return err
		}
		world.Id = uid.String()
	}
	err = this.replaceWorld(world)
	if err != nil {
		util.Logger.Error("unable to replace world", attributes.ErrorKey, err, "world", world.Id)
	}
	return err
}

func (this *StateRepo) DevGetWorld(id string) (world WorldMsg, exist bool, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.devGetWorldLocked(id)
}

// devGetWorldLocked is DevGetWorld for a caller that holds this.mux, read or write.
func (this *StateRepo) devGetWorldLocked(id string) (world WorldMsg, exist bool, err error) {
	worldp, exist := this.Worlds[id]
	if !exist {
		return world, exist, nil
	}
	world, err = snapshotLocked(worldp)
	return world, exist, err
}

// snapshotLocked converts world under its mutex; the caller holds this.mux.
func snapshotLocked(world *World) (WorldMsg, error) {
	world.mux.Lock()
	defer world.mux.Unlock()
	return world.ToMsg()
}

func (this *StateRepo) DevDeleteWorld(id string) (err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if this.shutDown {
		return errShutDown
	}
	//stopped before the delete, so no run still holding the world can store it again
	err = this.Stop()
	if err != nil {
		util.Logger.Error("unable to stop change routines", attributes.ErrorKey, err)
		return err
	}
	//Start rebuilds from this.Worlds, so a failed delete resumes the world
	defer this.Start()
	return this.deleteWorldLocked(id)
}

// deleteWorldLocked deletes world id; the caller holds this.mux and has stopped the routines.
func (this *StateRepo) deleteWorldLocked(id string) (err error) {
	err = this.Persistence.DeleteWorld(id)
	if err != nil {
		return err
	}
	delete(this.Worlds, id)
	return nil
}

// Update for HTTP-DEV-API
// Stops all change routines and redeploys new world with new room
// requests a mutex lock on the state repo
func (this *StateRepo) DevUpdateRoom(worldId string, room RoomMsg) (err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if worldId == "" {
		return errors.New("missing world id")
	}
	world, exists := this.Worlds[worldId]
	if !exists {
		return errors.New("unknown world id")
	}
	snapshot, stopped, err := this.beginEditLocked(world)
	if stopped {
		defer this.Start()
	}
	if err != nil {
		// a failed conversion leaves world without its id, states and routines, and persisting that would store a broken world
		return err
	}
	model, err := withRoom(snapshot, room)
	if err != nil {
		return err
	}
	return this.storeWorldLocked(model)
}

// withRoom is the change DevUpdateRoom makes to a world snapshot, converted but not stored.
func withRoom(world WorldMsg, room RoomMsg) (result World, err error) {
	if world.Rooms == nil {
		world.Rooms = map[string]RoomMsg{}
	}
	if room.Id == "" {
		uid, err := uuid.NewRandom()
		if err != nil {
			util.Logger.Error("unable to generate room id", attributes.ErrorKey, err)
			return result, err
		}
		room.Id = uid.String()
	}
	world.Rooms[room.Id] = room
	return world.ToModel()
}

// Update for HTTP-DEV-API
// Stops all change routines and redeploys new world with new room and device
// requests a mutex lock on the state repo
func (this *StateRepo) DevUpdateDevice(worldId string, roomId string, device DeviceMsg) (err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if worldId == "" {
		return errors.New("missing world id")
	}
	if this.Worlds == nil {
		this.Worlds = map[string]*World{}
	}
	world, exists := this.Worlds[worldId]
	if !exists {
		return errors.New("unknown world id")
	}
	snapshot, stopped, err := this.beginEditLocked(world)
	if stopped {
		defer this.Start()
	}
	if err != nil {
		// a failed conversion leaves world without its id, states and routines, and persisting that would store a broken world
		return err
	}
	model, err := withDevice(snapshot, roomId, device)
	if err != nil {
		return err
	}
	return this.storeWorldLocked(model)
}

// withDevice is the change DevUpdateDevice makes to a world snapshot, converted but not stored.
func withDevice(world WorldMsg, roomId string, device DeviceMsg) (result World, err error) {
	if world.Rooms == nil {
		world.Rooms = map[string]RoomMsg{}
	}
	if roomId == "" {
		return result, errors.New("missing room id")
	}
	room, ok := world.Rooms[roomId]
	if !ok {
		return result, errors.New("unknown room id: " + roomId)
	}
	if room.Devices == nil {
		room.Devices = map[string]DeviceMsg{}
	}
	if device.Id == "" {
		uid, err := uuid.NewRandom()
		if err != nil {
			util.Logger.Error("unable to generate device id", attributes.ErrorKey, err)
			return result, err
		}
		device.Id = uid.String()
	}
	room.Devices[device.Id] = device
	world.Rooms[room.Id] = room
	return world.ToModel()
}

// Stops all change routines if any are running and loads state repo from the database (no restart of change routines)
func (this *StateRepo) Load() (err error) {
	err = this.Stop()
	if err != nil {
		util.Logger.Error("unable to stop change routines", attributes.ErrorKey, err)
		return err
	}
	this.MosesProtocolId, err = this.EnsureProtocol(this.Config.Protocol, []model.ProtocolSegment{{Name: this.Config.ProtocolSegmentName}})
	if err != nil {
		util.Logger.Error("unable to ensure protocol", attributes.ErrorKey, err)
		return err
	}
	this.Worlds, err = this.Persistence.LoadWorlds()
	if err != nil {
		return err
	}
	return nil
}

// stops all change routines; may be called repeatedly while already stopped ore not started
func (this *StateRepo) Stop() (err error) {
	for _, ticker := range this.changeRoutinesTickers {
		ticker.Stop()
	}
	for _, stop := range this.stopChannels {
		stop <- true
	}
	this.stopChannels = nil
	this.changeRoutinesTickers = nil
	this.changeRoutineIndex = nil
	this.externalRefDeviceIndex = nil
	this.serviceDeviceIndex = nil
	this.deviceRoomIndex = nil
	this.deviceWorldIndex = nil
	this.roomWorldIndex = nil
	return
}

// starts change routines; will first call stop() to prevent overpopulation of change routines
// if error occurs, the state repo may be in a partially running state which can not be stopped with Stop()
// in this case a panic occurs
func (this *StateRepo) Start() {
	err := this.Stop()
	if err != nil {
		panic(err)
	}
	this.changeRoutineIndex = map[string]ChangeRoutineIndexElement{}
	this.externalRefDeviceIndex = map[string]*Device{}
	this.serviceDeviceIndex = map[string]*Device{}
	this.deviceRoomIndex = map[string]*Room{}
	this.deviceWorldIndex = map[string]*World{}
	this.roomWorldIndex = map[string]*World{}
	for id, world := range this.Worlds {
		if this.SkipWorldIds[id] || (world != nil && this.SkipWorldIds[world.Id]) {
			util.Logger.Info("world migrated to an environment, legacy runtime skips it", "world", id)
			continue
		}
		tickers, stops, err := this.StartWorld(world)
		if err != nil {
			panic(err)
		}
		this.changeRoutinesTickers = append(this.changeRoutinesTickers, tickers...)
		this.stopChannels = append(this.stopChannels, stops...)
	}

	//the command handler is NOT registered here any more. It used to be, and it
	//was re-registered on every Start(), which happens on every legacy api call.
	//The registration now lives in lib.New, because a command has to be offered
	//to the new runtime first and only fall back to this one: there is exactly
	//one handler on the connector, and it cannot belong to one of the two
	//runtimes. HandleCommand below is unchanged and is what the wiring calls.
	return
}

// ExternalRefWorldIds maps every platform device the legacy runtime currently
// acts on to the world it belongs to. It exists for one diagnostic the per world
// cutover cannot make on its own: an environment may reference the devices of a
// world that was never migrated, and then both runtimes publish for that device.
func (this *StateRepo) ExternalRefWorldIds() map[string]string {
	this.mux.RLock()
	defer this.mux.RUnlock()
	result := make(map[string]string, len(this.externalRefDeviceIndex))
	for ref, device := range this.externalRefDeviceIndex {
		if ref == "" || device == nil {
			continue
		}
		world, ok := this.deviceWorldIndex[device.Id]
		if !ok || world == nil {
			continue
		}
		result[ref] = world.Id
	}
	return result
}

// errShutDown refuses a world change after Shutdown, whose restart would outlive the service.
var errShutDown = errors.New("the legacy state repository is shut down")

// Shutdown stops every change routine for good. It takes this.mux, so it cannot run Stop alongside an update's
// Stop, where both would send on the same stop channel and the second send would block forever.
func (this *StateRepo) Shutdown() {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.shutDown = true
	_ = this.Stop()
}

// replaceWorld stores world and runs it in place of the running world with its id; the caller holds this.mux.
// The routines are stopped before the write: a run still holding the old world persists it on state.set, which must not land after the new one.
func (this *StateRepo) replaceWorld(world World) (err error) {
	if this.shutDown {
		return errShutDown
	}
	err = this.Stop()
	if err != nil {
		return err
	}
	//Start rebuilds from this.Worlds, so a failed write resumes the unchanged worlds
	defer this.Start()
	return this.storeWorldLocked(world)
}

// beginEditLocked stops the routines and converts world for an edit; the caller holds this.mux and calls Start when stopped is true.
// With the routines stopped no run holds the world mutex, so an edit waits for running scripts once, in Stop.
func (this *StateRepo) beginEditLocked(world *World) (snapshot WorldMsg, stopped bool, err error) {
	if this.shutDown {
		return snapshot, false, errShutDown
	}
	err = this.Stop()
	if err != nil {
		return snapshot, false, err
	}
	snapshot, err = snapshotLocked(world)
	return snapshot, true, err
}

// storeWorldLocked stores world and puts it in place of the running world with its id; the caller holds this.mux and has stopped the routines.
func (this *StateRepo) storeWorldLocked(world World) (err error) {
	err = this.persistWorld(world)
	if err != nil {
		return err
	}
	this.Worlds[world.Id] = &world
	return nil
}

// persists given world; will not stop any change routines, nor will it request a lock on the world mutex
func (this *StateRepo) persistWorld(world World) (err error) {
	return this.Persistence.PersistWorld(world)
}

func (this *StateRepo) sendSensorData(device *Device, service Service, value interface{}) {
	if this.Config.Debug {
		util.Logger.Debug("send sensor data", "device", device.Id, "service", service.Id, "value", value)
	}
	if device.ExternalRef == "" {
		util.Logger.Warn("no external ref for device", "device", device.Id)
		return
	}
	if service.ExternalRef == "" {
		util.Logger.Warn("no external ref for service", "service", service.Id)
		return
	}
	token, err := this.Connector.Security().Access()
	if err != nil {
		util.Logger.Error("unable to get access token", attributes.ErrorKey, err)
		return
	}

	msg := platform_connector_lib.CommandResponseMsg{}
	msgStr, err := json.Marshal(value)
	if err != nil {
		util.Logger.Error("unable to marshal sensor data", attributes.ErrorKey, err, "device", device.ExternalRef, "service", service.ExternalRef)
		return
	}
	msg[this.Config.ProtocolSegmentName] = string(msgStr)
	err = this.Connector.HandleDeviceEventWithAuthToken(token, device.ExternalRef, service.ExternalRef, msg, platform_connector_lib.Sync)
	if err != nil {
		util.Logger.Error("unable to send sensor data", attributes.ErrorKey, err, "device", device.ExternalRef, "service", service.ExternalRef)
	}
}

func (this *StateRepo) HandleCommand(externalDeviceRef string, externalServiceRef string, cmdMsg interface{}, responder func(respMsg interface{})) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	device, ok := this.externalRefDeviceIndex[externalDeviceRef]
	if !ok {
		util.Logger.Warn("no device with ref found", "external_ref", externalDeviceRef)
		return
	}
	world, ok := this.deviceWorldIndex[device.Id]
	if !ok {
		util.Logger.Warn("no world for device found", "device", device.Id, "external_ref", externalDeviceRef)
		return
	}
	room, ok := this.deviceRoomIndex[device.Id]
	if !ok {
		util.Logger.Warn("no room for device found", "device", device.Id, "external_ref", externalDeviceRef)
		return
	}

	for _, service := range device.Services {
		if service.ExternalRef == externalServiceRef {
			err := run(service.Code, this.getJsCommandApi(world, room, device, cmdMsg, responder), this.Config.JsTimeout, world.mux, this.Brake, world.Id, service.Id)
			if err != nil {
				util.Logger.Warn("command handling failed", attributes.ErrorKey, err, "device", device.Name, "service", service.Name)
			}
			return
		}
	}
	util.Logger.Warn("no matching service for device found", "external_ref", externalServiceRef)
}

func (this *StateRepo) RunService(serviceId string, cmdMsg interface{}) (resp interface{}, err error) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	device, ok := this.serviceDeviceIndex[serviceId]
	if !ok {
		util.Logger.Warn("no device with ref found", "service_id", serviceId)
		return
	}

	service, ok := device.Services[serviceId]
	if !ok {
		util.Logger.Warn("no service with id found", "service_id", serviceId)
		return
	}

	world, ok := this.deviceWorldIndex[device.Id]
	if !ok {
		util.Logger.Warn("no world for device found", "device", device.Id, "service_id", serviceId)
		return
	}
	room, ok := this.deviceRoomIndex[device.Id]
	if !ok {
		util.Logger.Warn("no room for device found", "device", device.Id, "service_id", serviceId)
		return
	}
	err = run(service.Code, this.getJsCommandApi(world, room, device, cmdMsg, func(respMsg interface{}) {
		resp = respMsg
	}), this.Config.JsTimeout, world.mux, this.Brake, world.Id, service.Id)
	return
}
