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


# Service Example:

## Moses-Device:
```
{
   "world":"bf8d36ec-cb3d-45a5-92c5-06a8f3234271",
   "room":"1711976a-9ed6-4833-8d9b-68f0740f3a4f",
   "device":{
      "id":"30ea7ee1-041a-41ca-8813-ebc770ca6524",
      "name":"test-lamp-1",
      "external_type_id":"urn:infai:ses:device-type:34a4b8d2-4e65-45a1-8c34-06a0c4294ed9",
      "external_ref":"urn:infai:ses:device:ce4d0300-6c3d-47c4-821f-323c3b9d1f55",
      "states":{
         "level":"off"
      },
      "change_routines":{
         
      },
      "services":{
         "52f1c328-e7ae-4b79-bf10-c57038378ed6":{
            "id":"52f1c328-e7ae-4b79-bf10-c57038378ed6",
            "name":"setOn",
            "external_ref":"urn:infai:ses:service:346d6397-9e0e-4d6f-a418-1540ea8cb5ae",
            "sensor_interval":0,
            "code":"moses.device.state.set(\"level\", \"on\");\nmoses.service.send({\"executed\": true });"
         },
         "589f6d84-7b7d-4583-917d-28f9ff52875a":{
            "id":"589f6d84-7b7d-4583-917d-28f9ff52875a",
            "name":"getState",
            "external_ref":"urn:infai:ses:service:34df4d8d-fdfe-42e5-88a5-74076decd534",
            "sensor_interval":15,
            "code":"var level = moses.device.state.get(\"level\");\nvar output = {\"payload\": {\"on\":level == \"on\"}};\nmoses.service.send(output);"
         },
         "f2b4492b-84a9-478f-8ec0-12c02f076e64":{
            "id":"f2b4492b-84a9-478f-8ec0-12c02f076e64",
            "name":"setOff",
            "external_ref":"urn:infai:ses:service:ce7f5435-d09e-4d9b-b066-5f81d07c5d94",
            "sensor_interval":0,
            "code":"moses.device.state.set(\"level\", \"off\");\nmoses.service.send({\"executed\": true });"
         }
      }
   }
}
```

## Device-Type:
```
{
   "id":"urn:infai:ses:device-type:34a4b8d2-4e65-45a1-8c34-06a0c4294ed9",
   "name":"Test Lamp Moses",
   "description":"",
   "services":[
      {
         "id":"urn:infai:ses:service:34df4d8d-fdfe-42e5-88a5-74076decd534",
         "local_id":"getState",
         "name":"getState",
         "description":"",
         "interaction":"event+request",
         "aspect_ids":[
            "urn:infai:ses:aspect:861227f6-1523-46a7-b8ab-a4e76f0bdd32",
            "urn:infai:ses:aspect:a7470d73-dde3-41fc-92bd-f16bb28f2da6"
         ],
         "protocol_id":"urn:infai:ses:protocol:3b59ea31-da98-45fd-a354-1b9bd06b837e",
         "inputs":[
            
         ],
         "outputs":[
            {
               "id":"urn:infai:ses:content:c579c816-de2e-4d99-9f35-fb4068d43c98",
               "content_variable":{
                  "id":"urn:infai:ses:content-variable:9d32286d-ec99-48bd-b581-f23d3fd6f9dd",
                  "name":"state",
                  "type":"https://schema.org/StructuredValue",
                  "sub_content_variables":[
                     {
                        "id":"urn:infai:ses:content-variable:ca0e3ef5-7194-4fc8-85fa-e27a89e1d28d",
                        "name":"on",
                        "type":"https://schema.org/Boolean",
                        "sub_content_variables":null,
                        "characteristic_id":"urn:infai:ses:characteristic:7dc1bb7e-b256-408a-a6f9-044dc60fdcf5",
                        "value":null,
                        "serialization_options":null
                     }
                  ],
                  "characteristic_id":"",
                  "value":null,
                  "serialization_options":null
               },
               "serialization":"json",
               "protocol_segment_id":"urn:infai:ses:protocol-segment:05f1467c-95c8-4a83-a1ed-1c8369fd158a"
            }
         ],
         "function_ids":[
            "urn:infai:ses:measuring-function:20d3c1d3-77d7-4181-a9f3-b487add58cd0"
         ],
         "rdf_type":""
      },
      {
         "id":"urn:infai:ses:service:ce7f5435-d09e-4d9b-b066-5f81d07c5d94",
         "local_id":"setOff",
         "name":"setOff",
         "description":"",
         "interaction":"request",
         "aspect_ids":[
            "urn:infai:ses:aspect:a7470d73-dde3-41fc-92bd-f16bb28f2da6",
            "urn:infai:ses:aspect:861227f6-1523-46a7-b8ab-a4e76f0bdd32"
         ],
         "protocol_id":"urn:infai:ses:protocol:3b59ea31-da98-45fd-a354-1b9bd06b837e",
         "inputs":[
            
         ],
         "outputs":[
            
         ],
         "function_ids":[
            "urn:infai:ses:controlling-function:2f35150b-9df7-4cad-95bc-165fa00219fd"
         ],
         "rdf_type":""
      },
      {
         "id":"urn:infai:ses:service:346d6397-9e0e-4d6f-a418-1540ea8cb5ae",
         "local_id":"setOn",
         "name":"setOn",
         "description":"",
         "interaction":"request",
         "aspect_ids":[
            "urn:infai:ses:aspect:861227f6-1523-46a7-b8ab-a4e76f0bdd32",
            "urn:infai:ses:aspect:a7470d73-dde3-41fc-92bd-f16bb28f2da6"
         ],
         "protocol_id":"urn:infai:ses:protocol:3b59ea31-da98-45fd-a354-1b9bd06b837e",
         "inputs":[
            
         ],
         "outputs":[
            
         ],
         "function_ids":[
            "urn:infai:ses:controlling-function:79e7914b-f303-4a7d-90af-dee70db05fd9"
         ],
         "rdf_type":""
      }
   ],
   "device_class_id":"urn:infai:ses:device-class:14e56881-16f9-4120-bb41-270a43070c86",
   "rdf_type":""
}
```
