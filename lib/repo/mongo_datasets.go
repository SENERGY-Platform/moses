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

import (
	"bytes"
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/gridfs"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// mongoDatasets keeps metadata in a collection and the raw file in a gridfs
// bucket under the dataset id, so the two cannot drift apart on lookup.
type mongoDatasets struct {
	collection *mongo.Collection
	bucket     *gridfs.Bucket
}

func (this *Mongo) Datasets() Datasets {
	return &mongoDatasets{collection: this.datasetCollection(), bucket: this.datasetBucket}
}

func (this *mongoDatasets) Create(ctx context.Context, meta DatasetMeta, raw []byte) error {
	//the file first: metadata pointing at a missing file is a broken dataset,
	//a file without metadata is only an orphan
	err := this.bucket.UploadFromStreamWithID(meta.Id, meta.Name, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	_, err = this.collection.InsertOne(ctx, meta)
	if err != nil {
		_ = this.bucket.DeleteContext(ctx, meta.Id)
		return err
	}
	return nil
}

func (this *mongoDatasets) Get(ctx context.Context, id string) (DatasetMeta, error) {
	result := DatasetMeta{}
	err := this.collection.FindOne(ctx, bson.M{"id": id}).Decode(&result)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return result, ErrNotFound
	}
	return result, err
}

func (this *mongoDatasets) ListByOwner(ctx context.Context, owner string) ([]DatasetMeta, error) {
	cursor, err := this.collection.Find(ctx, bson.M{"owner": owner}, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	result := []DatasetMeta{}
	err = cursor.All(ctx, &result)
	return result, err
}

func (this *mongoDatasets) All(ctx context.Context) ([]DatasetMeta, error) {
	cursor, err := this.collection.Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	result := []DatasetMeta{}
	err = cursor.All(ctx, &result)
	return result, err
}

func (this *mongoDatasets) Content(ctx context.Context, id string) ([]byte, error) {
	buffer := &bytes.Buffer{}
	_, err := this.bucket.DownloadToStream(id, buffer)
	if errors.Is(err, gridfs.ErrFileNotFound) {
		return nil, ErrNotFound
	}
	return buffer.Bytes(), err
}

func (this *mongoDatasets) Delete(ctx context.Context, id string) error {
	_, err := this.collection.DeleteOne(ctx, bson.M{"id": id})
	if err != nil {
		return err
	}
	err = this.bucket.DeleteContext(ctx, id)
	if errors.Is(err, gridfs.ErrFileNotFound) {
		return nil
	}
	return err
}
