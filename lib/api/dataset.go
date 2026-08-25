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

package api

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/util"
	sc_jwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func init() {
	datasetEndpoints = append(datasetEndpoints, DatasetEndpoints)
}

// maxDatasetUploadBytes bounds one upload. A year of one-minute samples in a
// verbose german export is around 20 MB, so 32 stays comfortable without
// inviting arbitrary files.
const maxDatasetUploadBytes = 32 << 20

// defaultDatasetTimezone interprets offsetless timestamps unless the upload
// says otherwise. German energy exports carry local time.
const defaultDatasetTimezone = "Europe/Berlin"

// DatasetEndpoints serves uploaded timeseries files. Datasets are immutable:
// there is no update route, a corrected file is a new dataset, so a channel
// referencing an id always replays the same data.
func DatasetEndpoints(config config.Config, datasets repo.Datasets, router gin.IRouter) {
	for _, route := range []func(repo.Datasets) (string, string, gin.HandlerFunc){
		postDatasetH,
		listDatasetsH,
		getDatasetH,
		deleteDatasetH,
	} {
		method, path, handler := route(datasets)
		router.Handle(method, path, handler)
	}
}

// @Summary Upload a timeseries dataset
// @Description The body is the file itself (CSV: header line, time column first, one or more named value columns). Dialect is detected: comma or semicolon separated, decimal point or comma, timestamps as RFC3339, "2006-01-02 15:04", "02.01.2006 15:04" or unix seconds. Offsetless timestamps are interpreted in the tz parameter's zone. The file is parsed before it is stored, so a broken file is refused here with a line number instead of playing silence later.
// @Tags Dataset
// @Accept plain
// @Produce json
// @Security Bearer
// @Param name query string true "display name of the dataset"
// @Param tz query string false "IANA timezone for offsetless timestamps (default Europe/Berlin)"
// @Param file body string true "the file content"
// @Success 201 {object} repo.DatasetMeta
// @Failure 400 {string} string "the file, the name or the timezone is unusable; parse errors carry the line"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 500 {string} string "error message"
// @Router /datasets [post]
func postDatasetH(datasets repo.Datasets) (string, string, gin.HandlerFunc) {
	return http.MethodPost, "/datasets", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		name := strings.TrimSpace(gc.Query("name"))
		if name == "" {
			gc.String(http.StatusBadRequest, "the name query parameter must be set")
			return
		}
		timezone := gc.Query("tz")
		if timezone == "" {
			timezone = defaultDatasetTimezone
		}
		location, err := time.LoadLocation(timezone)
		if err != nil {
			gc.String(http.StatusBadRequest, "unknown timezone %q", timezone)
			return
		}

		raw, err := io.ReadAll(http.MaxBytesReader(gc.Writer, gc.Request.Body, maxDatasetUploadBytes))
		if err != nil {
			gc.String(http.StatusBadRequest, "unable to read the upload (limit %d bytes): %s", maxDatasetUploadBytes, err.Error())
			return
		}

		series, err := dataset.ParseCSV(raw, location)
		if err != nil {
			gc.String(http.StatusBadRequest, "unable to parse the file: %s", err.Error())
			return
		}

		meta := repo.DatasetMeta{
			Id:          uuid.NewString(),
			Owner:       token.GetUserId(),
			Name:        name,
			Timezone:    timezone,
			SizeBytes:   int64(len(raw)),
			CreatedUnix: time.Now().Unix(),
		}
		for _, s := range series {
			meta.Columns = append(meta.Columns, repo.DatasetColumn{
				Name: s.Name, Points: len(s.Points), FromUnix: s.From(), ToUnix: s.To(),
			})
		}

		if err = datasets.Create(gc.Request.Context(), meta, raw); err != nil {
			util.Logger.Error("unable to store dataset", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to store dataset")
			return
		}
		gc.JSON(http.StatusCreated, meta)
	}
}

// @Summary List datasets
// @Description Every dataset owned by the caller, ordered by name. Empty list, never null.
// @Tags Dataset
// @Produce json
// @Security Bearer
// @Success 200 {array} repo.DatasetMeta
// @Failure 401 {string} string "the token carries no subject"
// @Failure 500 {string} string "error message"
// @Router /datasets [get]
func listDatasetsH(datasets repo.Datasets) (string, string, gin.HandlerFunc) {
	return http.MethodGet, "/datasets", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		//an admin sees every dataset, for the same reason as the environments
		list, err := listDatasetsFor(gc, datasets, token)
		if err != nil {
			util.Logger.Error("unable to list datasets", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to list datasets")
			return
		}
		gc.JSON(http.StatusOK, list)
	}
}

// @Summary Get one dataset's metadata
// @Tags Dataset
// @Produce json
// @Security Bearer
// @Param id path string true "dataset id"
// @Success 200 {object} repo.DatasetMeta
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "no such dataset, or no access to it"
// @Failure 500 {string} string "error message"
// @Router /datasets/{id} [get]
func getDatasetH(datasets repo.Datasets) (string, string, gin.HandlerFunc) {
	return http.MethodGet, "/datasets/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		meta, err := requireDataset(gc, datasets, token)
		if err != nil {
			return
		}
		gc.JSON(http.StatusOK, meta)
	}
}

// @Summary Delete one dataset
// @Description A channel still referencing the deleted dataset stops playing on its next reload and is reported there.
// @Tags Dataset
// @Security Bearer
// @Param id path string true "dataset id"
// @Success 204 {string} string "deleted, or there was nothing to delete"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "the dataset belongs to somebody else"
// @Failure 500 {string} string "error message"
// @Router /datasets/{id} [delete]
func deleteDatasetH(datasets repo.Datasets) (string, string, gin.HandlerFunc) {
	return http.MethodDelete, "/datasets/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		meta, err := datasets.Get(gc.Request.Context(), gc.Param("id"))
		switch {
		case errors.Is(err, repo.ErrNotFound):
			gc.Status(http.StatusNoContent) //nothing to delete
			return
		case err != nil:
			util.Logger.Error("unable to read dataset", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read dataset")
			return
		case meta.Owner != token.GetUserId() && !token.IsAdmin():
			gc.String(http.StatusNotFound, "not found")
			return
		}
		if err = datasets.Delete(gc.Request.Context(), gc.Param("id")); err != nil {
			util.Logger.Error("unable to delete dataset", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to delete dataset")
			return
		}
		gc.Status(http.StatusNoContent)
	}
}

// requireDataset answers 404 for a missing dataset and, deliberately with the
// same status, for somebody else's: existence is not information a caller
// without access should get.
func listDatasetsFor(gc *gin.Context, datasets repo.Datasets, token sc_jwt.Token) ([]repo.DatasetMeta, error) {
	if token.IsAdmin() {
		return datasets.All(gc.Request.Context())
	}
	return datasets.ListByOwner(gc.Request.Context(), token.GetUserId())
}

func requireDataset(gc *gin.Context, datasets repo.Datasets, token sc_jwt.Token) (repo.DatasetMeta, error) {
	meta, err := datasets.Get(gc.Request.Context(), gc.Param("id"))
	switch {
	case errors.Is(err, repo.ErrNotFound):
		gc.String(http.StatusNotFound, "not found")
		return meta, err
	case err != nil:
		util.Logger.Error("unable to read dataset", attributes.ErrorKey, err)
		gc.String(http.StatusInternalServerError, "unable to read dataset")
		return meta, err
	case meta.Owner != token.GetUserId() && !token.IsAdmin():
		gc.String(http.StatusNotFound, "not found")
		return meta, repo.ErrNotFound
	}
	return meta, nil
}
