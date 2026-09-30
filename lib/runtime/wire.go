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

// The frames between a supervisor and one environment worker (SNRGY-4817,
// variant B spike). One gob stream per direction; store-shaped payloads travel
// as bson, the encoding the store keeps them in, because gob drops empty maps.

import (
	"bufio"
	"encoding/gob"
	"errors"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SENERGY-Platform/moses/lib/crashbrake"
	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"go.mongodb.org/mongo-driver/bson"
)

const (
	wireStart       = "start"      // parent->child: Blob = bson definition; reply
	wireProfile     = "profile"    // parent->child: Blob, From, To, Flags; reply Profile
	wireSetState    = "set_state"  // parent->child: Env, Blob = bson StateChange; reply
	wireSnapshot    = "snapshot"   // parent->child: Env; reply Blob = bson StateSnapshot
	wireStop        = "stop"       // parent->child; reply, then the child exits
	wireReply       = "reply"      // either direction, Id echoes the request
	wireReadings    = "readings"   // child->parent: a batch of readings to publish
	wireAcks        = "acks"       // parent->child: the outcome of each reading
	wireBeatKind    = "beat"       // child->parent: liveness and the oldest script run in flight
	wireLoadState   = "load_state" // child->parent: Env; reply Blob = bson RuntimeState
	wireSaveState   = "save_state" // child->parent: Blob = bson RuntimeState; reply
	wireDeleteState = "del_state"  // child->parent: Env; reply
	wireTimeShape   = "time_shape" // child->parent: Device, Service; reply Shape
	wireCheckpoint  = "checkpoint" // child->parent: Blob = bson HistoryJobProgress; reply
)

// wireProfileCheckpoints in Flags makes a profile run forward a checkpoint at
// every chunk boundary, as a stored run would.
const wireProfileCheckpoints = 1

type wireMsg struct {
	Kind     string
	Id       uint64
	Err      *wireError
	Env      string
	Device   string
	Service  string
	Blob     []byte
	From     int64
	To       int64
	Flags    uint32
	Shape    *devices.TimeShape
	Readings []wireReading
	Acks     []wireAck
	Beat     *wireBeat
	Profile  *wireProfileResult

	// Decisions is what the worker's own crash brake read at its boot: the
	// channel the previous incarnation of this worker crashed in.
	Decisions []crashbrake.Decision
}

// wireReading carries a number inline and anything else as bson, so an empty
// map stays an empty map and not a nil the publish would marshal as null.
type wireReading struct {
	Seq     uint64
	Device  string
	Service string
	IsNum   bool
	Num     float64
	Blob    []byte
	At      int64 // unix nanos
	Timed   bool  // PublishEventAt rather than PublishEvent
}

type wireAck struct {
	Seq uint64
	Err *wireError
}

type wireBeat struct {
	InRunNanos int64 // age of the oldest script run in flight, 0 when none
	Runs       int64 // script runs completed so far
}

type wireProfileResult struct {
	ElapsedNanos    int64
	Channels        int
	Steps           int64
	Published       int64
	Failed          int64
	Checkpoints     int
	CheckpointBytes int64
}

// wireError keeps the sentinels the runtime and the api test with errors.Is.
type wireError struct {
	Code string
	Msg  string
}

var wireSentinels = map[string]error{
	"not_running":     repo.ErrNotRunning,
	"not_found":       repo.ErrNotFound,
	"history_running": ErrHistoryRunning,
	"no_time_path":    devices.ErrNoTimePath,
	"unusable_shape":  devices.ErrUnusableTimeShape,
	"publish_aborted": ErrPublishAborted,
}

func toWireError(err error) *wireError {
	if err == nil {
		return nil
	}
	for code, sentinel := range wireSentinels {
		if errors.Is(err, sentinel) {
			return &wireError{Code: code, Msg: err.Error()}
		}
	}
	return &wireError{Msg: err.Error()}
}

type remoteError struct {
	msg      string
	sentinel error
}

func (this *remoteError) Error() string { return this.msg }
func (this *remoteError) Unwrap() error { return this.sentinel }

func fromWireError(err *wireError) error {
	if err == nil {
		return nil
	}
	return &remoteError{msg: err.Msg, sentinel: wireSentinels[err.Code]}
}

// encodeReadingValue takes the fast path for a finite float64 only; every other
// value, NaN included, goes through bson so its Go type survives the trip.
func encodeReadingValue(value interface{}, reading *wireReading) error {
	if number, ok := value.(float64); ok && !math.IsNaN(number) && !math.IsInf(number, 0) {
		reading.IsNum = true
		reading.Num = number
		return nil
	}
	blob, err := bson.Marshal(bson.M{"v": value})
	if err != nil {
		return err
	}
	reading.Blob = blob
	return nil
}

func decodeReadingValue(reading wireReading) (interface{}, error) {
	if reading.IsNum {
		return reading.Num, nil
	}
	var holder struct {
		V interface{} `bson:"v"`
	}
	if err := bson.Unmarshal(reading.Blob, &holder); err != nil {
		return nil, err
	}
	return plainBsonValue(holder.V), nil
}

// plainBsonValue turns bson's decoded containers back into the plain maps and
// slices the runtime produces, so a published value marshals as it did in process.
func plainBsonValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case bson.D:
		result := make(map[string]interface{}, len(typed))
		for _, element := range typed {
			result[element.Key] = plainBsonValue(element.Value)
		}
		return result
	case bson.M:
		result := make(map[string]interface{}, len(typed))
		for key, element := range typed {
			result[key] = plainBsonValue(element)
		}
		return result
	case bson.A:
		result := make([]interface{}, len(typed))
		for i, element := range typed {
			result[i] = plainBsonValue(element)
		}
		return result
	case map[string]interface{}:
		//the driver decodes a document below a map as a map, but an array in it as bson.A
		for key, element := range typed {
			typed[key] = plainBsonValue(element)
		}
		return typed
	}
	return value
}

// countingWriter and countingReader measure the bytes on the pipe itself.
type countingWriter struct {
	w     io.Writer
	bytes *atomic.Int64
}

func (this countingWriter) Write(p []byte) (int, error) {
	n, err := this.w.Write(p)
	this.bytes.Add(int64(n))
	return n, err
}

type countingReader struct {
	r     io.Reader
	bytes *atomic.Int64
}

func (this countingReader) Read(p []byte) (int, error) {
	n, err := this.r.Read(p)
	this.bytes.Add(int64(n))
	return n, err
}

// wireStats are the IPC counters of one connection.
type wireStats struct {
	SentBytes, RecvBytes atomic.Int64
	SentMsgs, RecvMsgs   atomic.Int64
	Batches, Batched     atomic.Int64 // reading or ack frames, and the items in them
}

type wireConn struct {
	encMux sync.Mutex
	buf    *bufio.Writer
	enc    *gob.Encoder
	dec    *gob.Decoder
	stats  *wireStats
	broken atomic.Bool
}

func newWireConn(r io.Reader, w io.Writer) *wireConn {
	stats := &wireStats{}
	buf := bufio.NewWriterSize(countingWriter{w: w, bytes: &stats.SentBytes}, 64<<10)
	return &wireConn{
		buf:   buf,
		enc:   gob.NewEncoder(buf),
		dec:   gob.NewDecoder(bufio.NewReaderSize(countingReader{r: r, bytes: &stats.RecvBytes}, 64<<10)),
		stats: stats,
	}
}

func (this *wireConn) write(msg *wireMsg) error {
	this.encMux.Lock()
	defer this.encMux.Unlock()
	if this.broken.Load() {
		return io.ErrClosedPipe
	}
	err := this.enc.Encode(msg)
	if err == nil {
		err = this.buf.Flush()
	}
	if err != nil {
		//a gob stream cannot resync after a partial frame, so the connection is done
		this.broken.Store(true)
		return err
	}
	this.stats.SentMsgs.Add(1)
	return nil
}

func (this *wireConn) read() (*wireMsg, error) {
	msg := &wireMsg{}
	if err := this.dec.Decode(msg); err != nil {
		return nil, err
	}
	this.stats.RecvMsgs.Add(1)
	return msg, nil
}

// wireOutbox is the single writer of a connection. Control frames go out one by
// one; readings (child) and acks (parent) are coalesced into one frame of
// whatever is queued at that moment, up to wireMaxBatch. In hold mode readings
// wait for flushNow, which the history loop reaches once per instant, or for
// holdLimit, whichever comes first.
type wireOutbox struct {
	conn     *wireConn
	control  chan *wireMsg
	readings chan wireReading
	acks     chan wireAck
	flush    chan struct{}
	done     chan struct{}
	stop     sync.Once

	hold      atomic.Bool
	holdLimit time.Duration
}

const wireMaxBatch = 512

func newWireOutbox(conn *wireConn) *wireOutbox {
	return &wireOutbox{
		conn:      conn,
		control:   make(chan *wireMsg, 256),
		readings:  make(chan wireReading, 4096),
		acks:      make(chan wireAck, 4096),
		flush:     make(chan struct{}, 1),
		done:      make(chan struct{}),
		holdLimit: time.Millisecond,
	}
}

func (this *wireOutbox) close() { this.stop.Do(func() { close(this.done) }) }

// flushNow asks for the held readings to go out; it never blocks.
func (this *wireOutbox) flushNow() {
	select {
	case this.flush <- struct{}{}:
	default:
	}
}

func (this *wireOutbox) send(msg *wireMsg) bool {
	select {
	case this.control <- msg:
		return true
	case <-this.done:
		return false
	}
}

func (this *wireOutbox) sendReading(reading wireReading) bool {
	select {
	case this.readings <- reading:
		return true
	case <-this.done:
		return false
	}
}

func (this *wireOutbox) sendAck(ack wireAck) bool {
	select {
	case this.acks <- ack:
		return true
	case <-this.done:
		return false
	}
}

// run writes until the outbox is closed or the pipe breaks; onBroken is told once.
func (this *wireOutbox) run(onBroken func(error)) {
	var held []wireReading
	var deadline *time.Timer
	var expired <-chan time.Time
	disarm := func() {
		if deadline != nil {
			deadline.Stop()
		}
		deadline, expired = nil, nil
	}
	writeHeld := func() error {
		disarm()
		if len(held) == 0 {
			return nil
		}
		batch := held
		held = nil
		this.conn.stats.Batches.Add(1)
		this.conn.stats.Batched.Add(int64(len(batch)))
		return this.conn.write(&wireMsg{Kind: wireReadings, Readings: batch})
	}
	for {
		var err error
		select {
		case <-this.done:
			disarm()
			return
		case msg := <-this.control:
			err = this.conn.write(msg)
		case first := <-this.readings:
			held = append(held, first)
		drainReadings:
			for len(held) < wireMaxBatch {
				select {
				case next := <-this.readings:
					held = append(held, next)
				default:
					break drainReadings
				}
			}
			switch {
			case !this.hold.Load() || len(held) >= wireMaxBatch:
				err = writeHeld()
			case deadline == nil:
				deadline = time.NewTimer(this.holdLimit)
				expired = deadline.C
			}
		case <-this.flush:
			err = writeHeld()
		case <-expired:
			deadline, expired = nil, nil
			err = writeHeld()
		case first := <-this.acks:
			batch := []wireAck{first}
		drainAcks:
			for len(batch) < wireMaxBatch {
				select {
				case next := <-this.acks:
					batch = append(batch, next)
				default:
					break drainAcks
				}
			}
			this.conn.stats.Batches.Add(1)
			this.conn.stats.Batched.Add(int64(len(batch)))
			err = this.conn.write(&wireMsg{Kind: wireAcks, Acks: batch})
		}
		if err != nil {
			disarm()
			this.close()
			if onBroken != nil {
				onBroken(err)
			}
			return
		}
	}
}

// wireCalls correlates requests with their replies on one side of a connection.
type wireCalls struct {
	mux     sync.Mutex
	next    uint64
	pending map[uint64]chan *wireMsg
	dead    error
}

func (this *wireCalls) open() (uint64, chan *wireMsg, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if this.dead != nil {
		return 0, nil, this.dead
	}
	if this.pending == nil {
		this.pending = map[uint64]chan *wireMsg{}
	}
	this.next++
	reply := make(chan *wireMsg, 1)
	this.pending[this.next] = reply
	return this.next, reply, nil
}

func (this *wireCalls) forget(id uint64) {
	this.mux.Lock()
	delete(this.pending, id)
	this.mux.Unlock()
}

func (this *wireCalls) deliver(msg *wireMsg) {
	this.mux.Lock()
	reply, known := this.pending[msg.Id]
	delete(this.pending, msg.Id)
	this.mux.Unlock()
	if known {
		reply <- msg
	}
}

// fail answers every open call with err and refuses new ones.
func (this *wireCalls) fail(err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if this.dead == nil {
		this.dead = err
	}
	for id, reply := range this.pending {
		reply <- &wireMsg{Kind: wireReply, Id: id, Err: toWireError(err)}
		delete(this.pending, id)
	}
}
