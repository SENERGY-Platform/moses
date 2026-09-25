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
	"net/http"

	"github.com/gin-gonic/gin"
)

// maxDocumentBytes bounds a request carrying a whole document: an environment,
// its state, or a legacy world, room or device. The Musterwerke environment is
// ~260 KB, so 16 MiB leaves ample room while bounding what is buffered and parsed.
const maxDocumentBytes = 16 << 20

// maxRequestBytes bounds a request carrying at most one script or a few fields;
// a script is limited to 32 KiB, so 1 MiB is far more than any needs.
const maxRequestBytes = 1 << 20

// bindLimitedJSON decodes the request body into dst, reading at most limit bytes.
// It answers 413 for a larger body and 400 with prefix and the error for any
// other failure, and reports whether the handler may continue.
func bindLimitedJSON(gc *gin.Context, dst interface{}, limit int64, prefix string) bool {
	gc.Request.Body = http.MaxBytesReader(gc.Writer, gc.Request.Body, limit)
	err := gc.ShouldBindJSON(dst)
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		gc.String(http.StatusRequestEntityTooLarge, "the request body is larger than the %d byte limit", limit)
		return false
	}
	gc.String(http.StatusBadRequest, "%s%s", prefix, err.Error())
	return false
}
