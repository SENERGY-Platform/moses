# My Own Smart Environment Simulator (MOSES)
A simulator for smart environments

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
- for rooms the moses object provides APIs for the current room and the world the room is assigned to with `moses.world` and `moses.room`
- for devices the moses object provides APIs for the current device and the world and room the device is assigned to with `moses.world`, `moses.room` and `moses.device`

each sub Api has a state object, which can be read and changed with a `get()` and `set()` method.

the world sub API can access room sub APIs of its children (ref State-Hierarchies) with `getRoom(id)`.
the room sub API can access device sub APIs of its children (ref State-Hierarchies) with  `getDevice(id)`.

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
