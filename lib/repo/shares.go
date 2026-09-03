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

package repo

import "context"

// ShareSet names the accounts the platform devices of one environment are
// shared with.
//
// It lives beside the definition rather than in it, like the runtime state: the
// set is a grant on platform resources and not content of the document, and a
// document written from a copy that was read before a share would otherwise put
// the old set back - silently, and with the rights on the devices left standing.
type ShareSet struct {
	EnvironmentId string   `json:"environment_id" bson:"environment_id"`
	Users         []string `json:"users" bson:"users"`
	Groups        []string `json:"groups" bson:"groups"`

	// Version is the compare-and-swap counter of this set: Load hands out the
	// stored one and Save writes only while it still matches, so two shares of
	// one environment cannot both think they replaced the other's set. Zero
	// means nothing is stored yet, and Save then insists on creating it.
	Version int64 `json:"version" bson:"version"`

	// Set by the store, not by callers.
	UpdatedAtUnix int64 `json:"updated_at_unix" bson:"updated_at_unix"`
}

// Empty reports whether the set grants nothing.
func (this ShareSet) Empty() bool {
	return len(this.Users) == 0 && len(this.Groups) == 0
}

// Shares stores, per environment, who its devices are shared with.
type Shares interface {
	// Load returns an empty set if none is stored yet.
	Load(ctx context.Context, environmentId string) (ShareSet, error)

	// Save writes the whole set, but only while the stored version still is
	// shares.Version - zero meaning "nothing is stored yet". It reports a
	// *VersionConflictError otherwise and returns the version the set carries
	// afterwards, which is what a second write of the same call passes back in.
	Save(ctx context.Context, shares ShareSet) (version int64, err error)

	// Delete tolerates a missing id. It removes the record only; the rights on
	// the devices are withdrawn by whoever deletes them.
	Delete(ctx context.Context, environmentId string) error
}
