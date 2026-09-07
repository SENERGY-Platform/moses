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
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/util"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// HistoryJobRunning is the one state string the store itself interprets, in
// Running(). It has to stay the string of runtime.HistoryRunning.
const HistoryJobRunning = "running"

// MongoHistoryJobs is the HistoryJobs half of the store, on the connection of
// the Mongo it came from. A type of its own for the same reason MongoStates is
// one: the two Delete methods mean different things.
type MongoHistoryJobs struct {
	store *Mongo
}

var _ HistoryJobs = &MongoHistoryJobs{}

// HistoryJobs returns the history job store on the same connection.
func (this *Mongo) HistoryJobs() *MongoHistoryJobs {
	return &MongoHistoryJobs{store: this}
}

// Save replaces the whole record, definition included. It is the write at the
// start and at the end of a run; the writes in between are Checkpoint, which
// leaves the definition alone.
func (this *MongoHistoryJobs) Save(ctx context.Context, record HistoryJobRecord) error {
	if strings.TrimSpace(record.EnvironmentId) == "" {
		return fmt.Errorf("%w: history job", ErrMissingId)
	}
	//set by the store, as documented on the field. the wall clock is correct
	//here: this is a timestamp that outlives the process, not a duration.
	record.UpdatedAtUnix = time.Now().Unix()
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	err := upsert(ctx, this.store.historyJobCollection(), bson.M{"environment_id": record.EnvironmentId}, record)
	if err != nil {
		util.Logger.Error("unable to save the history job", attributes.ErrorKey, err, "environment_id", record.EnvironmentId)
	}
	return err
}

// Checkpoint writes what one chunk boundary produced and never the definition,
// so an edit made during the run cannot change the run that is going on. The
// checkpoint is replaced only when it carries a position, the resume count is
// cleared only at a boundary, and a missing record is reported rather than
// created: a checkpoint without the record that Save wrote is a run nothing
// knows the window of.
func (this *MongoHistoryJobs) Checkpoint(ctx context.Context, environmentId string, progress HistoryJobProgress) error {
	if strings.TrimSpace(environmentId) == "" {
		return fmt.Errorf("%w: history job", ErrMissingId)
	}
	set := bson.M{
		"published":       progress.Published,
		"failed":          progress.Failed,
		"last_error":      progress.LastError,
		"updated_at_unix": time.Now().Unix(),
	}
	update := bson.M{"$set": set}
	if progress.Position.IsZero() {
		//stored as a datetime, a zero time is the year 1 and would be read back
		//as a position the run reached. absent is what "no position yet" means,
		//and one update must not both set and unset the same path.
		update["$unset"] = bson.M{"position": ""}
	} else {
		set["position"] = progress.Position
	}
	if !progress.Checkpoint.Position.IsZero() {
		//a checkpoint is the instant it stands at, so one without an instant is
		//not a chunk boundary: the stored one stays, because resuming from the
		//year 1 would replay the whole window instead of the last chunk
		set["checkpoint"] = progress.Checkpoint
	}
	if progress.AtBoundary {
		//a run that reached a boundary has got going, so the count starts over:
		//the guard bounds the resumes that reached none, not the resumes ever
		set["resumes"] = 0
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	result, err := this.store.historyJobCollection().UpdateOne(ctx, bson.M{"environment_id": environmentId}, update)
	if err != nil {
		util.Logger.Error("unable to checkpoint the history job", attributes.ErrorKey, err, "environment_id", environmentId)
		return err
	}
	if result.MatchedCount == 0 {
		return fmt.Errorf("%w: history job of %v", ErrNotFound, environmentId)
	}
	return nil
}

func (this *MongoHistoryJobs) Load(ctx context.Context, environmentId string) (result HistoryJobRecord, err error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	err = this.store.historyJobCollection().FindOne(ctx, bson.M{"environment_id": environmentId}).Decode(&result)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return HistoryJobRecord{}, fmt.Errorf("%w: history job of %v", ErrNotFound, environmentId)
	}
	if err != nil {
		util.Logger.Error("unable to load the history job", attributes.ErrorKey, err, "environment_id", environmentId)
		return HistoryJobRecord{}, err
	}
	result.Checkpoint.Initialise()
	return result, nil
}

// Running is the resume list of a starting service, so it scans with the load
// timeout and skips an undecodable document instead of failing the start - the
// same reasoning as Environments.All.
func (this *MongoHistoryJobs) Running(ctx context.Context) (result []HistoryJobRecord, err error) {
	result = []HistoryJobRecord{}
	ctx, cancel := withLoadTimeout(ctx)
	defer cancel()
	cursor, err := this.store.historyJobCollection().Find(ctx, bson.M{"state": HistoryJobRunning})
	if err != nil {
		util.Logger.Error("unable to list the running history jobs", attributes.ErrorKey, err)
		return result, err
	}
	defer cursor.Close(context.Background())
	for cursor.Next(ctx) {
		record := HistoryJobRecord{}
		err = cursor.Decode(&record)
		if err != nil {
			util.Logger.Error("skipping undecodable history job document", attributes.ErrorKey, err)
			continue
		}
		record.Checkpoint.Initialise()
		result = append(result, record)
	}
	err = cursor.Err()
	if err != nil {
		util.Logger.Error("unable to list the running history jobs", attributes.ErrorKey, err)
		return result, err
	}
	return result, nil
}

func (this *MongoHistoryJobs) Delete(ctx context.Context, environmentId string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := this.store.historyJobCollection().DeleteMany(ctx, bson.M{"environment_id": environmentId})
	if err != nil {
		util.Logger.Error("unable to delete the history job", attributes.ErrorKey, err, "environment_id", environmentId)
	}
	return err
}
