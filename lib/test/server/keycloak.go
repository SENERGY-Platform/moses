/*
 * Copyright 2019 InfAI (CC SES)
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

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SENERGY-Platform/platform-connector-lib/security"
	"github.com/julienschmidt/httprouter"
	"log"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"sync"
	"time"
)

func Keycloak(ctx context.Context, wg *sync.WaitGroup) (authEndpoint string, err error) {
	router, err := getRouter()
	if err != nil {
		return "", err
	}
	server := httptest.NewServer(router)
	wg.Add(1)
	go func() {
		<-ctx.Done()
		server.Close()
		wg.Done()
	}()
	return server.URL, nil
}

func getRouter() (router *httprouter.Router, err error) {
	defer func() {
		if r := recover(); r != nil && err == nil {
			log.Printf("%s: %s", r, debug.Stack())
			err = errors.New(fmt.Sprint("Recovered Error: ", r))
		}
	}()
	router = httprouter.New()

	router.POST("/auth/realms/master/protocol/openid-connect/token", func(writer http.ResponseWriter, request *http.Request, params httprouter.Params) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		username := request.FormValue("username")
		var token string
		if request.FormValue("grant_type") == "password" && username == "" {
			http.Error(writer, "missing user name", 400)
			return
		}
		if username == "admin" {
			token = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJqdGkiOiJiOGUyNGZkNy1jNjJlLTRhNWQtOTQ4ZC1mZGI2ZWVkM2JmYzYiLCJleHAiOjE1MzA1MzIwMzIsIm5iZiI6MCwiaWF0IjoxNTMwNTI4NDMyLCJpc3MiOiJodHRwczovL2F1dGguc2VwbC5pbmZhaS5vcmcvYXV0aC9yZWFsbXMvbWFzdGVyIiwiYXVkIjoiZnJvbnRlbmQiLCJzdWIiOiJhZG1pbiIsInR5cCI6IkJlYXJlciIsImF6cCI6ImZyb250ZW5kIiwibm9uY2UiOiIyMmUwZWNmOC1mOGExLTQ0NDUtYWYyNy00ZDUzYmY1ZDE4YjkiLCJhdXRoX3RpbWUiOjE1MzA1Mjg0MjMsInNlc3Npb25fc3RhdGUiOiIxZDc1YTk4NC03MzU5LTQxYmUtODFiOS03MzJkODI3NGMyM2UiLCJhY3IiOiIwIiwiYWxsb3dlZC1vcmlnaW5zIjpbIioiXSwicmVhbG1fYWNjZXNzIjp7InJvbGVzIjpbImNyZWF0ZS1yZWFsbSIsImFkbWluIiwiZGV2ZWxvcGVyIiwidW1hX2F1dGhvcml6YXRpb24iLCJ1c2VyIl19LCJyZXNvdXJjZV9hY2Nlc3MiOnsibWFzdGVyLXJlYWxtIjp7InJvbGVzIjpbInZpZXctaWRlbnRpdHktcHJvdmlkZXJzIiwidmlldy1yZWFsbSIsIm1hbmFnZS1pZGVudGl0eS1wcm92aWRlcnMiLCJpbXBlcnNvbmF0aW9uIiwiY3JlYXRlLWNsaWVudCIsIm1hbmFnZS11c2VycyIsInF1ZXJ5LXJlYWxtcyIsInZpZXctYXV0aG9yaXphdGlvbiIsInF1ZXJ5LWNsaWVudHMiLCJxdWVyeS11c2VycyIsIm1hbmFnZS1ldmVudHMiLCJtYW5hZ2UtcmVhbG0iLCJ2aWV3LWV2ZW50cyIsInZpZXctdXNlcnMiLCJ2aWV3LWNsaWVudHMiLCJtYW5hZ2UtYXV0aG9yaXphdGlvbiIsIm1hbmFnZS1jbGllbnRzIiwicXVlcnktZ3JvdXBzIl19LCJhY2NvdW50Ijp7InJvbGVzIjpbIm1hbmFnZS1hY2NvdW50IiwibWFuYWdlLWFjY291bnQtbGlua3MiLCJ2aWV3LXByb2ZpbGUiXX19LCJyb2xlcyI6WyJ1bWFfYXV0aG9yaXphdGlvbiIsImFkbWluIiwiY3JlYXRlLXJlYWxtIiwiZGV2ZWxvcGVyIiwidXNlciIsIm9mZmxpbmVfYWNjZXNzIl0sIm5hbWUiOiJkZiBkZmZmZiIsInByZWZlcnJlZF91c2VybmFtZSI6InNlcGwiLCJnaXZlbl9uYW1lIjoiZGYiLCJmYW1pbHlfbmFtZSI6ImRmZmZmIiwiZW1haWwiOiJzZXBsQHNlcGwuZGUifQ.kefK08rDs_PR4Ayzgh4nbK9SDKbNbQLZgH-NyPvysU4"
		} else if username == "user" {
			token = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJqdGkiOiJiOGUyNGZkNy1jNjJlLTRhNWQtOTQ4ZC1mZGI2ZWVkM2JmYzYiLCJleHAiOjE1MzA1MzIwMzIsIm5iZiI6MCwiaWF0IjoxNTMwNTI4NDMyLCJpc3MiOiJodHRwczovL2F1dGguc2VwbC5pbmZhaS5vcmcvYXV0aC9yZWFsbXMvbWFzdGVyIiwiYXVkIjoiZnJvbnRlbmQiLCJzdWIiOiJ1c2VyIiwidHlwIjoiQmVhcmVyIiwiYXpwIjoiZnJvbnRlbmQiLCJub25jZSI6IjIyZTBlY2Y4LWY4YTEtNDQ0NS1hZjI3LTRkNTNiZjVkMThiOSIsImF1dGhfdGltZSI6MTUzMDUyODQyMywic2Vzc2lvbl9zdGF0ZSI6IjFkNzVhOTg0LTczNTktNDFiZS04MWI5LTczMmQ4Mjc0YzIzZSIsImFjciI6IjAiLCJhbGxvd2VkLW9yaWdpbnMiOlsiKiJdLCJyZWFsbV9hY2Nlc3MiOnsicm9sZXMiOlsiY3JlYXRlLXJlYWxtIiwiZGV2ZWxvcGVyIiwidW1hX2F1dGhvcml6YXRpb24iLCJ1c2VyIl19LCJyZXNvdXJjZV9hY2Nlc3MiOnsibWFzdGVyLXJlYWxtIjp7InJvbGVzIjpbInZpZXctaWRlbnRpdHktcHJvdmlkZXJzIiwidmlldy1yZWFsbSIsIm1hbmFnZS1pZGVudGl0eS1wcm92aWRlcnMiLCJpbXBlcnNvbmF0aW9uIiwiY3JlYXRlLWNsaWVudCIsIm1hbmFnZS11c2VycyIsInF1ZXJ5LXJlYWxtcyIsInZpZXctYXV0aG9yaXphdGlvbiIsInF1ZXJ5LWNsaWVudHMiLCJxdWVyeS11c2VycyIsIm1hbmFnZS1ldmVudHMiLCJtYW5hZ2UtcmVhbG0iLCJ2aWV3LWV2ZW50cyIsInZpZXctdXNlcnMiLCJ2aWV3LWNsaWVudHMiLCJtYW5hZ2UtYXV0aG9yaXphdGlvbiIsIm1hbmFnZS1jbGllbnRzIiwicXVlcnktZ3JvdXBzIl19LCJhY2NvdW50Ijp7InJvbGVzIjpbIm1hbmFnZS1hY2NvdW50IiwibWFuYWdlLWFjY291bnQtbGlua3MiLCJ2aWV3LXByb2ZpbGUiXX19LCJyb2xlcyI6WyJ1bWFfYXV0aG9yaXphdGlvbiIsImNyZWF0ZS1yZWFsbSIsImRldmVsb3BlciIsInVzZXIiLCJvZmZsaW5lX2FjY2VzcyJdLCJuYW1lIjoidXNlciIsInByZWZlcnJlZF91c2VybmFtZSI6InNlcGwiLCJnaXZlbl9uYW1lIjoiZGYiLCJmYW1pbHlfbmFtZSI6ImRmZmZmIiwiZW1haWwiOiJzZXBsQHNlcGwuZGUifQ.ieB5XGFQkvCrd3mINQoiSCrIEylz5GVoe-y7HiPDj9c"
		} else {
			token = "eyJhbGciOiJSUzI1NiIsInR5cCIgOiAiSldUIiwia2lkIiA6ICIzaUtabW9aUHpsMmRtQnBJdS1vSkY4ZVVUZHh4OUFIckVOcG5CcHM5SjYwIn0.eyJqdGkiOiJiOGUyNGZkNy1jNjJlLTRhNWQtOTQ4ZC1mZGI2ZWVkM2JmYzYiLCJleHAiOjE1MzA1MzIwMzIsIm5iZiI6MCwiaWF0IjoxNTMwNTI4NDMyLCJpc3MiOiJodHRwczovL2F1dGguc2VwbC5pbmZhaS5vcmcvYXV0aC9yZWFsbXMvbWFzdGVyIiwiYXVkIjoiZnJvbnRlbmQiLCJzdWIiOiJkZDY5ZWEwZC1mNTUzLTQzMzYtODBmMy03ZjQ1NjdmODVjN2IiLCJ0eXAiOiJCZWFyZXIiLCJhenAiOiJmcm9udGVuZCIsIm5vbmNlIjoiMjJlMGVjZjgtZjhhMS00NDQ1LWFmMjctNGQ1M2JmNWQxOGI5IiwiYXV0aF90aW1lIjoxNTMwNTI4NDIzLCJzZXNzaW9uX3N0YXRlIjoiMWQ3NWE5ODQtNzM1OS00MWJlLTgxYjktNzMyZDgyNzRjMjNlIiwiYWNyIjoiMCIsImFsbG93ZWQtb3JpZ2lucyI6WyIqIl0sInJlYWxtX2FjY2VzcyI6eyJyb2xlcyI6WyJjcmVhdGUtcmVhbG0iLCJhZG1pbiIsImRldmVsb3BlciIsInVtYV9hdXRob3JpemF0aW9uIiwidXNlciJdfSwicmVzb3VyY2VfYWNjZXNzIjp7Im1hc3Rlci1yZWFsbSI6eyJyb2xlcyI6WyJ2aWV3LWlkZW50aXR5LXByb3ZpZGVycyIsInZpZXctcmVhbG0iLCJtYW5hZ2UtaWRlbnRpdHktcHJvdmlkZXJzIiwiaW1wZXJzb25hdGlvbiIsImNyZWF0ZS1jbGllbnQiLCJtYW5hZ2UtdXNlcnMiLCJxdWVyeS1yZWFsbXMiLCJ2aWV3LWF1dGhvcml6YXRpb24iLCJxdWVyeS1jbGllbnRzIiwicXVlcnktdXNlcnMiLCJtYW5hZ2UtZXZlbnRzIiwibWFuYWdlLXJlYWxtIiwidmlldy1ldmVudHMiLCJ2aWV3LXVzZXJzIiwidmlldy1jbGllbnRzIiwibWFuYWdlLWF1dGhvcml6YXRpb24iLCJtYW5hZ2UtY2xpZW50cyIsInF1ZXJ5LWdyb3VwcyJdfSwiYWNjb3VudCI6eyJyb2xlcyI6WyJtYW5hZ2UtYWNjb3VudCIsIm1hbmFnZS1hY2NvdW50LWxpbmtzIiwidmlldy1wcm9maWxlIl19fSwicm9sZXMiOlsidW1hX2F1dGhvcml6YXRpb24iLCJhZG1pbiIsImNyZWF0ZS1yZWFsbSIsImRldmVsb3BlciIsInVzZXIiLCJvZmZsaW5lX2FjY2VzcyJdLCJuYW1lIjoiZGYgZGZmZmYiLCJwcmVmZXJyZWRfdXNlcm5hbWUiOiJzZXBsIiwiZ2l2ZW5fbmFtZSI6ImRmIiwiZmFtaWx5X25hbWUiOiJkZmZmZiIsImVtYWlsIjoic2VwbEBzZXBsLmRlIn0.eOwKV7vwRrWr8GlfCPFSq5WwR_p-_rSJURXCV1K7ClBY5jqKQkCsRL2V4YhkP1uS6ECeSxF7NNOLmElVLeFyAkvgSNOUkiuIWQpMTakNKynyRfH0SrdnPSTwK2V1s1i4VjoYdyZWXKNjeT2tUUX9eCyI5qOf_Dzcai5FhGCSUeKpV0ScUj5lKrn56aamlW9IdmbFJ4VwpQg2Y843Vc0TqpjK9n_uKwuRcQd9jkKHkbwWQ-wyJEbFWXHjQ6LnM84H0CQ2fgBqPPfpQDKjGSUNaCS-jtBcbsBAWQSICwol95BuOAqVFMucx56Wm-OyQOuoQ1jaLt2t-Uxtr-C9wKJWHQ"
		}
		err = json.NewEncoder(writer).Encode(security.OpenidToken{
			AccessToken:      token,
			ExpiresIn:        10 * float64(time.Hour),
			RefreshExpiresIn: 5 * float64(time.Hour),
			RefreshToken:     "",
			TokenType:        "",
			RequestTime:      time.Now(),
		})
		if err != nil {
			log.Println("ERROR: unable to encode response", err)
		}
	})

	router.GET("/auth/admin/realms/master/users", func(writer http.ResponseWriter, request *http.Request, params httprouter.Params) {
		username := request.URL.Query().Get("username")
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		err = json.NewEncoder(writer).Encode([]security.UserRepresentation{{
			Id:   username,
			Name: username,
		}})
		if err != nil {
			log.Println("ERROR: unable to encode response", err)
		}
	})

	router.GET("/auth/admin/realms/master/users/:userid/role-mappings/realm", func(writer http.ResponseWriter, request *http.Request, params httprouter.Params) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		if params.ByName("userid") == "admin" {
			err = json.NewEncoder(writer).Encode([]security.RoleMapping{{Name: "admin"}})
		} else {
			err = json.NewEncoder(writer).Encode([]security.RoleMapping{{Name: "test-group"}})
		}
		if err != nil {
			log.Println("ERROR: unable to encode response", err)
		}
	})

	return
}
