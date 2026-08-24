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
	"errors"
	"testing"
)

func testMeta(id string, owner string, name string) DatasetMeta {
	return DatasetMeta{
		Id: id, Owner: owner, Name: name, Timezone: "Europe/Berlin",
		Columns:     []DatasetColumn{{Name: "power", Points: 2, FromUnix: 100, ToUnix: 200}},
		SizeBytes:   3,
		CreatedUnix: 1000,
	}
}

func TestDatasetsRoundTripMetadataAndContent(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	datasets := store.Datasets()

	raw := []byte("t,v\n1,1\n2,2\n")
	if err := datasets.Create(ctx, testMeta("d1", "user-a", "Lastgang"), raw); err != nil {
		t.Fatal(err)
	}

	meta, err := datasets.Get(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "Lastgang" || meta.Timezone != "Europe/Berlin" || len(meta.Columns) != 1 {
		t.Errorf("metadata did not survive: %+v", meta)
	}

	content, err := datasets.Content(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, raw) {
		t.Errorf("the raw file has to come back byte for byte, got %q", content)
	}
}

func TestDatasetsRefuseADuplicateId(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	datasets := store.Datasets()
	if err := datasets.Create(ctx, testMeta("d1", "user-a", "eins"), []byte("a")); err != nil {
		t.Fatal(err)
	}
	//datasets are immutable, so a second create under the same id must fail
	//rather than replace - replay reproducibility depends on it
	if err := datasets.Create(ctx, testMeta("d1", "user-a", "zwei"), []byte("b")); err == nil {
		t.Fatal("a duplicate id has to be refused")
	}
	meta, err := datasets.Get(ctx, "d1")
	if err != nil || meta.Name != "eins" {
		t.Errorf("the original has to survive the refused duplicate: %v %+v", err, meta)
	}
}

func TestDatasetsListByOwnerAndNotFound(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	datasets := store.Datasets()
	_ = datasets.Create(ctx, testMeta("d2", "user-a", "zwei"), []byte("b"))
	_ = datasets.Create(ctx, testMeta("d1", "user-a", "eins"), []byte("a"))
	_ = datasets.Create(ctx, testMeta("d3", "user-b", "fremd"), []byte("c"))

	list, err := datasets.ListByOwner(ctx, "user-a")
	if err != nil || len(list) != 2 || list[0].Name != "eins" {
		t.Errorf("expected the owner's two datasets ordered by name, got %v %+v", err, list)
	}
	if _, err = datasets.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	if _, err = datasets.Content(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for missing content, got %v", err)
	}
}

func TestDatasetsDeleteRemovesBothHalves(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	datasets := store.Datasets()
	if err := datasets.Create(ctx, testMeta("d1", "user-a", "weg"), []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := datasets.Delete(ctx, "d1"); err != nil {
		t.Fatal(err)
	}
	if _, err := datasets.Get(ctx, "d1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("metadata survived the delete: %v", err)
	}
	if _, err := datasets.Content(ctx, "d1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("content survived the delete: %v", err)
	}
	//deleting nothing is not an error
	if err := datasets.Delete(ctx, "d1"); err != nil {
		t.Errorf("a second delete has to be silent, got %v", err)
	}
}
