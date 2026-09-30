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

// Package platformhttp builds the http clients moses calls the device-manager and
// the device-repository with.
package platformhttp

import (
	"net/http"
	"time"
)

// DefaultTimeout applies where the configured read timeout is zero or less,
// which would otherwise mean no bound at all.
const DefaultTimeout = 10 * time.Second

// WriteTimeout bounds one device-manager write. A write the platform may already
// have applied has to end with its answer, so it gets as long as a whole api
// mutation (lib/api mutationTimeout) and not the read timeout.
const WriteTimeout = 5 * time.Minute

// Clients are the reads and the writes of the platform calls. Both use the
// default transport, so they share its connections.
type Clients struct {
	// Read carries the reads; its timeout covers the whole exchange, body included.
	Read *http.Client
	// Write carries the device-manager writes; its timeout is only the backstop
	// for a context without a deadline, the caller's deadline is what bounds it.
	Write *http.Client
}

// NewClients returns the reads bounded by readTimeout and the writes by WriteTimeout.
func NewClients(readTimeout time.Duration) Clients {
	return Clients{Read: New(readTimeout), Write: &http.Client{Timeout: WriteTimeout}}
}

// Reads is the read client, or one with the default timeout where none is set.
func (this Clients) Reads() *http.Client {
	if this.Read == nil {
		return New(0)
	}
	return this.Read
}

// Writes is the write client, or one bounded by WriteTimeout where none is set.
func (this Clients) Writes() *http.Client {
	if this.Write == nil {
		return &http.Client{Timeout: WriteTimeout}
	}
	return this.Write
}

// New returns a read client whose timeout covers the whole exchange, body
// included, so a service that accepts the connection and never answers cannot hold the caller.
func New(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &http.Client{Timeout: timeout}
}
