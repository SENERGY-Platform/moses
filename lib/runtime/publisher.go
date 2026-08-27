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

package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/devices"
	platform_connector_lib "github.com/SENERGY-Platform/platform-connector-lib"
)

// EventTimeKey is the entry moses puts into an EventMsg to say when the reading
// was taken. The connector's EventTimeProvider reads it and removes it again,
// so it never reaches a protocol segment.
//
// An EventMsg is keyed by protocol segment name, so this key shares a namespace
// with them. The slash makes a collision implausible, and lib.New refuses to
// start with a protocol segment of this name rather than leaving the collision
// to be discovered in production.
const EventTimeKey = "moses/event-time-unix-nano"

// EventTimeProvider is what lib.New hands the connector. It answers the one
// question the connector asks before it produces a record: which timestamp the
// kafka record carries.
//
// This is NOT what stamps the row in timescale. The timescale ingestion never
// sees this value; it reads the time out of the payload at the service's
// senergy/time_path, which is why a backfilled reading has to carry its time in
// the message body as well. See docs/backfill.md.
func EventTimeProvider(msg platform_connector_lib.EventMsg) (platform_connector_lib.EventMsg, time.Time) {
	raw, carried := msg[EventTimeKey]
	if !carried {
		return msg, time.Now()
	}
	//removed either way: it is not a protocol segment, and leaving it in would
	//show up in a diagnostic dump of the message
	delete(msg, EventTimeKey)
	nanos, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return msg, time.Now()
	}
	return msg, time.Unix(0, nanos)
}

// eventPublisher is the only thing the runtime needs the connector for.
type eventPublisher interface {
	// PublishEvent sends a reading taken now.
	PublishEvent(externalDeviceRef string, externalServiceRef string, value interface{}) error

	// PublishEventAt sends a reading taken at a given instant. It only reaches
	// timescale under that instant for a service whose TimeShapeOf resolves;
	// for every other service the platform stamps the arrival time, which is
	// why the backfill checks first and does not simply try.
	PublishEventAt(externalDeviceRef string, externalServiceRef string, value interface{}, at time.Time) error

	// TimeShapeOf reports whether the platform reads the event time out of the
	// payload of this service, and where. devices.ErrNoTimePath means it does
	// not; any other error names why a declared time path cannot be served.
	TimeShapeOf(externalDeviceRef string, externalServiceRef string) (devices.TimeShape, error)
}

// connectorPublisher publishes like the legacy sendSensorData. The envelope
// shape is what the platform's marshaller reads, so a migrated channel must
// produce the same bytes for the same script.
//
// A service that declares a time path is the exception, and deliberately so:
// there the bytes have to be an object carrying the value and the time, on the
// live path as much as on the backfill. Neither of the other two options works.
// A bare value sent to such a service is rejected by the platform's message
// cleaning on every event, because the root of that service is a record and a
// number is not one - so such a channel never worked here. An object with the
// time left out fares no better: the cleaning defaults the missing member to
// null, and the ingestion cannot read a time out of that, so it drops the row
// and notifies the device's owners - once per reading. Until
// platform-connector-lib c8133d0 it asserted that null to an int64 instead and
// panicked in a goroutine with no recover; the shape moses has to send is the
// same either way. lib/devices/ingestion_test.go pins both.
type connectorPublisher struct {
	connector   *platform_connector_lib.Connector
	segmentName string

	// shapes caches the resolved time shape per service. The lookup behind it is
	// a device and a device type read, which the connector performs again for
	// every event anyway; without the cache a publish would pay for it twice.
	// The ttl mirrors what the platform's own ingestion caches the same fact
	// for, so a changed device type takes effect equally fast on both sides.
	shapesMux sync.RWMutex
	shapes    map[string]cachedShape
}

type cachedShape struct {
	shape    devices.TimeShape
	err      error
	resolved time.Time
}

// shapeCacheTtl matches the five minutes platform-connector-lib caches the time
// characteristic of a service for.
const shapeCacheTtl = 5 * time.Minute

func (this *connectorPublisher) PublishEvent(externalDeviceRef string, externalServiceRef string, value interface{}) error {
	return this.PublishEventAt(externalDeviceRef, externalServiceRef, value, time.Now())
}

func (this *connectorPublisher) PublishEventAt(externalDeviceRef string, externalServiceRef string, value interface{}, at time.Time) error {
	token, err := this.connector.Security().Access()
	if err != nil {
		return err
	}
	body, err := this.body(externalDeviceRef, externalServiceRef, value, at)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	msg := platform_connector_lib.EventMsg{}
	msg[this.segmentName] = string(payload)
	msg[EventTimeKey] = strconv.FormatInt(at.UnixNano(), 10)
	return this.connector.HandleDeviceEventWithAuthToken(token, externalDeviceRef, externalServiceRef, msg, platform_connector_lib.Sync)
}

// body is the value as the service wants it: bare for a service without a time
// path, an object carrying value and time for one with a usable one.
//
// A declared but unusable time path is refused rather than published around. A
// bare value sent to such a service fails in the platform's ingestion on every
// single event, and that failure notifies the device's owners each time - one
// error here is cheaper than a notification per reading.
func (this *connectorPublisher) body(deviceRef string, serviceRef string, value interface{}, at time.Time) (interface{}, error) {
	shape, err := this.TimeShapeOf(deviceRef, serviceRef)
	switch {
	case errors.Is(err, devices.ErrNoTimePath):
		return value, nil
	case err != nil:
		return nil, err
	}
	number, numeric := asFloat(value)
	if !numeric {
		return nil, fmt.Errorf("the service reads its event time from the payload, so it needs a number, but this channel produced %T", value)
	}
	return shape.Payload(number, at), nil
}

func (this *connectorPublisher) TimeShapeOf(externalDeviceRef string, externalServiceRef string) (devices.TimeShape, error) {
	key := externalDeviceRef + "\x00" + externalServiceRef
	this.shapesMux.RLock()
	cached, known := this.shapes[key]
	this.shapesMux.RUnlock()
	if known && time.Since(cached.resolved) < shapeCacheTtl {
		return cached.shape, cached.err
	}

	service, err := this.service(externalDeviceRef, externalServiceRef)
	if err != nil {
		//not cached: a device repository that was briefly unreachable must not
		//keep a channel silent for the whole ttl
		return devices.TimeShape{}, err
	}
	shape, shapeErr := devices.ResolveTimeShape(service)

	this.shapesMux.Lock()
	if this.shapes == nil {
		this.shapes = map[string]cachedShape{}
	}
	this.shapes[key] = cachedShape{shape: shape, err: shapeErr, resolved: time.Now()}
	this.shapesMux.Unlock()
	return shape, shapeErr
}

// service reads the platform service a channel publishes to, through the same
// cache and with the same token the publish itself uses: whatever this can see,
// the publish can see too.
func (this *connectorPublisher) service(deviceRef string, serviceRef string) (models.Service, error) {
	token, err := this.connector.Security().Access()
	if err != nil {
		return models.Service{}, err
	}
	cache := this.connector.IotCache.WithToken(token)
	device, err := cache.GetDevice(deviceRef)
	if err != nil {
		return models.Service{}, fmt.Errorf("unable to read the platform device: %w", err)
	}
	deviceType, err := cache.GetDeviceType(device.DeviceTypeId)
	if err != nil {
		return models.Service{}, fmt.Errorf("unable to read the device type: %w", err)
	}
	for _, service := range deviceType.Services {
		if service.Id == serviceRef {
			return service, nil
		}
	}
	return models.Service{}, fmt.Errorf("the device type %v has no service %v", device.DeviceTypeId, serviceRef)
}

// deviceStateLogger reports a simulated device as online, as the legacy
// StartDevice does; without it a migrated device shows as offline after the
// cutover. Only connect, matching legacy, which never logs a disconnect.
type deviceStateLogger interface {
	LogDeviceConnect(id string) error
}
