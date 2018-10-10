# My Own Smart Environment Simulator (MOSES)
A simulator for smart environments

## Dependencies
* Persists data on Mongodb
* Calls the Iot-Repository from the SEPL-Project
* Publishes messages to Kafka (+Zookeeper)
* tests create docker containers
    * testing host needs access to fgseitsrancher.wifa.intern.uni-leipzig.de:5000/iot-ontology docker image
    * testing host needs access to fgseitsrancher.wifa.intern.uni-leipzig.de:5000/iot-device-repository docker image
    * testing host needs access to fgseitsrancher.wifa.intern.uni-leipzig.de:5000/permissionsearch docker image
* Golang library dependencies are managed by go.mod file

## State-Hierarchies 

### world

### room

### device

## Change-Routines
A user can define change routines for each world, room and device. 
These change routines will be called in user-defined intervals. The behavior of a routine is defined by ES5-JavaScript.
Which will be interpreted by Otto (https://github.com/robertkrimen/otto). Moses provides a API for the JS environment 
which allows to read and write the state of the world, room and device.

### JS-API
The API is accessed by the variable `moses` which provides sub APIs depending on, for which component the routine is written.

- for worlds the moses object provides a API for the world with `moses.world`.
- for rooms the moses object provides APIs for the current room and world with `moses.world` and `moses.room`
- for devices the moses object provides APIs for the current device, room and world with `moses.world`, `moses.room` and `moses.device`
- for services the moses object provides APIs for the current device, room and world with `moses.world`, `moses.room` and `moses.device`

each of these sub Api has a state object, which can be read and changed with a `get()` and `set()` method.

the world sub API can access room sub APIs of its children (ref State-Hierarchies) with `getRoom(id)`.
the room sub API can access device sub APIs of its children (ref State-Hierarchies) with  `getDevice(id)`.

services have additionally to these state-apis access to a service api object which allows access to the input variable with `moses.service.input`.
routinely called sensor-services have access to a send function with `moses.service.send()` but there input variable is `null`.
services which are called from outside have a input variable if one is send. they can respond with `moses.service.send()`.

#### World-Api
- world: object //world-sub-api of current world

#### Room-Api
- world: object //world-sub-api of current world
- room: object //room-sub-api of current room

#### Device-Api
- world: object //world-sub-api of current world
- room: object //room-sub-api of current room
- device: object //device-sub-api of current device

#### Sensor-Service-Api
- world: object //world-sub-api of current world
- room: object //room-sub-api of current room
- device: object //device-sub-api of current device
- service: object //sensor-sub-api

#### Actuator-Service-Api
- world: object //world-sub-api of current world
- room: object //room-sub-api of current room
- device: object //device-sub-api of current device
- service: object //actuator-sub-api

---------------------

#### World-Sub-Api
- state: object //state-sub-api
- getRoom: function(string)object //room-sub-api for given room id

#### Room-Sub-Api
- state: object //state-sub-api
- getDevice: function(string)object //device-sub-api for given device id

#### Device-Sub-Api
- state: object //state-sub-api

#### Sensor-Sub-Api
- send: function(anything)  //sends data to outside world
- input: null               //if service is called by timer as sensor, no input parameter is given

#### Actuator-Sub-Api
- send: function(anything)  //sends data to outside world
- input: anything           //input parameter from outside world call

#### State-Sub-Api
- set: function(string, anything) //set state value
- get: function(string) //get state value


### Example

```
//Example for Device-Change-Routine
//the answer is 42!
var deviceAnswer = moses.device.state.get("answer");    //42
moses.room.state.set("answer", deviceAnswer);
moses.world.state.set("answer", deviceAnswer);
var roomCounter = moses.room.state.get("counter");
roomCounter = roomCounter + 1;
moses.room.state.set("counter", roomCounter);
```

```
//Example for World-Change-Routine
//room temperature is influenced by the world
var temperature = moses.world.state.get("temp");
var room_temperature = moses.world.getRoom("room_1").state.get("temp");
if(temperature > room_temperature){
    room_temperature = room_temperature + 1;
}else if(temperature < room_temperature){
    room_temperature = room_temperature - 1;
}
moses.world.getRoom("room1").state.set("temp", room_temperature);
```

```
//Example for Sensor-Service
//reads room temperature
var temp = moses.room.state.get("temp");
moses.service.send(temp);
```

```
//Example for Actuator-Service
//increases room temperature and responds with new temperature
var temp = moses.room.state.get("temp");
temp = temp + moses.service.input.temp;
moses.room.state.set("temp", temp);
moses.service.send({"newtemp":temp});
```

