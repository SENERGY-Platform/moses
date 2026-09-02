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

// DatasetColumn describes one value column of an upload, taken from the parse
// at upload time.
type DatasetColumn struct {
	Name     string `json:"name" bson:"name"`
	Points   int    `json:"points" bson:"points"`
	FromUnix int64  `json:"from_unix" bson:"from_unix"`
	ToUnix   int64  `json:"to_unix" bson:"to_unix"`
}

// DatasetMeta is what a caller sees of an uploaded dataset. The raw file lives
// in gridfs; Timezone records how offsetless timestamps were interpreted at
// upload time, so a later re-parse cannot silently read them differently.
type DatasetMeta struct {
	Id string `json:"id" bson:"id"`
	// Owner is the uploader's user id, decided by the server; read-only for callers.
	Owner       string          `json:"owner,omitempty" bson:"owner"`
	Name        string          `json:"name" bson:"name"`
	Timezone    string          `json:"timezone" bson:"timezone"`
	Columns     []DatasetColumn `json:"columns" bson:"columns"`
	SizeBytes   int64           `json:"size_bytes" bson:"size_bytes"`
	CreatedUnix int64           `json:"created_unix" bson:"created_unix"`
}

// Datasets stores uploaded timeseries files. Datasets are immutable: replay
// stays reproducible because the data under an id can never change, so there
// is no update - a corrected file is a new dataset.
type Datasets interface {
	// Create stores the metadata and the raw file under meta.Id.
	Create(ctx context.Context, meta DatasetMeta, raw []byte) error

	// Get returns ErrNotFound if no such dataset exists.
	Get(ctx context.Context, id string) (DatasetMeta, error)

	// ListByOwner is ordered by name.
	ListByOwner(ctx context.Context, owner string) ([]DatasetMeta, error)

	// All returns every dataset, ordered by name. It serves the admin view.
	All(ctx context.Context) ([]DatasetMeta, error)

	// Content returns the raw uploaded file.
	Content(ctx context.Context, id string) ([]byte, error)

	// Delete removes metadata and file, and tolerates a missing id. A channel
	// referencing the deleted dataset stops playing on its next reload and is
	// reported there, not here - the store cannot know the references.
	Delete(ctx context.Context, id string) error
}
