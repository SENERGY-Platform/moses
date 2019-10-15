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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/ory/dockertest"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
	"time"
)

func Keycloak(pool *dockertest.Pool) (closer func(), hostPort string, ipAddress string, err error) {
	log.Println("start keycloak")
	keycloak, err := pool.Run("fgseitsrancher.wifa.intern.uni-leipzig.de:5000/keycloak", "dev", []string{
		"KEYCLOAK_USER=sepl",
		"KEYCLOAK_PASSWORD=sepl",
		"PROXY_ADDRESS_FORWARDING=true",
	})
	if err != nil {
		return func() {}, "", "", err
	}
	hostPort = keycloak.GetPort("8080/tcp")
	err = pool.Retry(func() error {
		//get admin access token
		form := url.Values{}
		form.Add("username", "sepl")
		form.Add("password", "sepl")
		form.Add("grant_type", "password")
		form.Add("client_id", "admin-cli")
		resp, err := http.Post(
			"http://"+keycloak.Container.NetworkSettings.IPAddress+":8080/auth/realms/master/protocol/openid-connect/token",
			"application/x-www-form-urlencoded",
			strings.NewReader(form.Encode()))
		if err != nil {
			log.Println("unable to request admin token", err)
			return err
		}
		tokenMsg := map[string]interface{}{}
		err = json.NewDecoder(resp.Body).Decode(&tokenMsg)
		if err != nil {
			log.Println("unable to decode admin token", err)
			return err
		}
		return nil
	})
	return func() { keycloak.Close() }, hostPort, keycloak.Container.NetworkSettings.IPAddress, err
}

func ConfigKeycloak(url string, exportLocation string, clientName string) (err error) {
	token, err := GetKeycloakAdminToken(url)
	if err != nil {
		return err
	}
	client := http.Client{
		Timeout: 5 * time.Second,
	}
	file, err := os.Open(exportLocation)
	if err != nil {
		log.Println("error on config load: ", err)
		return err
	}
	req, err := http.NewRequest("POST", url+"/auth/admin/realms/master/partialImport", file)
	if err != nil {
		return err
	}
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	req.WithContext(ctx)
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := ioutil.ReadAll(resp.Body)
		err = errors.New(resp.Status + ": " + string(b))
		return
	}
	clientId, err := KeycloakGetClientId(token, url, clientName)
	if err != nil {
		return err
	}
	log.Println("DEBUG: clientId=", clientId)
	serviceAccountUser, err := KeycloakGetServiceAccountUser(token, url, clientId)
	if err != nil {
		return err
	}
	err = KeycloakSetClientToAdmin(token, url, serviceAccountUser)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return err
	}
	return nil
}

func KeycloakGetServiceAccountUser(token string, keycloakUrl string, clientId string) (serviceAccountUser string, err error) {
	//http://fgseitsrancher.wifa.intern.uni-leipzig.de:8087/auth/admin/realms/master/clients/{{clientId}}/service-account-user  -> .id
	client := http.Client{
		Timeout: 5 * time.Second,
	}
	req, err := http.NewRequest("GET", keycloakUrl+"/auth/admin/realms/master/clients/"+url.PathEscape(clientId)+"/service-account-user", nil)
	if err != nil {
		return serviceAccountUser, err
	}
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	req.WithContext(ctx)
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return serviceAccountUser, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := ioutil.ReadAll(resp.Body)
		err = errors.New(resp.Status + ": " + string(b))
		return
	}
	temp := map[string]interface{}{}
	err = json.NewDecoder(resp.Body).Decode(&temp)
	if err != nil {
		return serviceAccountUser, err
	}
	serviceAccountUser, _ = temp["id"].(string)
	return serviceAccountUser, nil
}

func KeycloakGetClientId(token string, keycloakUrl string, clientName string) (id string, err error) {
	client := http.Client{
		Timeout: 5 * time.Second,
	}
	req, err := http.NewRequest("GET", keycloakUrl+"/auth/admin/realms/master/clients?clientId="+url.QueryEscape(clientName), nil)
	if err != nil {
		return id, err
	}
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	req.WithContext(ctx)
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return id, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := ioutil.ReadAll(resp.Body)
		err = errors.New(resp.Status + ": " + string(b))
		return
	}
	temp := []map[string]interface{}{}
	err = json.NewDecoder(resp.Body).Decode(&temp)
	if err != nil {
		return id, err
	}
	if len(temp) == 0 {
		return id, errors.New("no keycloak client found")
	}
	if len(temp) > 1 {
		log.Println("ERROR: unexpected response", temp)
		debug.PrintStack()
		return id, errors.New("to much keycloak clients found")
	}
	id, _ = temp[0]["id"].(string)
	return id, nil
}

func KeycloakGetAdminRole(token string, keycloakUrl string, serviceAccountUser string) (result interface{}, err error) {
	//http://keycloak:8080/auth/admin/realms/master/users/47cc38a4-5cd1-4676-a128-fb251c02e5ff/role-mappings/realm/available ->
	//[{"id":"b672608b-2f1c-4d97-890d-8fc23194564b","name":"developer","composite":false,"clientRole":false,"containerId":"master"},{"id":"7a04a6dd-1086-4e89-a825-60c2e7d50db9","name":"user","composite":false,"clientRole":false,"containerId":"master"}]

	client := http.Client{
		Timeout: 5 * time.Second,
	}
	req, err := http.NewRequest("GET", keycloakUrl+"/auth/admin/realms/master/users/"+url.PathEscape(serviceAccountUser)+"/role-mappings/realm/available", nil)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return nil, err
	}
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	req.WithContext(ctx)
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		responseBody, _ := ioutil.ReadAll(resp.Body)
		log.Println("KeycloakGetAdminRole() = ", string(responseBody))
		err = errors.New(resp.Status + ": " + string(responseBody))
		log.Println("ERROR:", err)
		debug.PrintStack()
		return
	}
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return nil, err
	}
	return result, nil
}

func KeycloakSetClientToAdmin(token string, keycloakUrl string, serviceAccountUser string) (err error) {
	//POST http://keycloak:8080/auth/admin/realms/{{realm}}/users/{{serviceAccountUser}}/role-mappings/realm
	//[{"id":"e0945a55-3f2c-4a56-9357-3c2f3016071d","name":"admin","description":"${role_offline-access}","composite":false,"clientRole":false,"containerId":"test"}]
	role, err := KeycloakGetAdminRole(token, keycloakUrl, serviceAccountUser)
	if err != nil {
		return err
	}
	client := http.Client{
		Timeout: 5 * time.Second,
	}
	b := new(bytes.Buffer)
	err = json.NewEncoder(b).Encode(role)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return err
	}
	req, err := http.NewRequest("POST", keycloakUrl+"/auth/admin/realms/master/users/"+url.PathEscape(serviceAccountUser)+"/role-mappings/realm", b)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return err
	}
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	req.WithContext(ctx)
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		debug.PrintStack()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		responseBody, _ := ioutil.ReadAll(resp.Body)
		err = errors.New(resp.Status + ": " + string(responseBody))
		log.Println("ERROR:", err)
		debug.PrintStack()
		return
	}
	return nil
}

func GetKeycloakAdminToken(authEndpoint string) (token string, err error) {
	client := http.Client{
		Timeout: 5 * time.Second,
	}
	resp, err := client.PostForm(authEndpoint+"/auth/realms/master/protocol/openid-connect/token", url.Values{
		"username":   {"sepl"},
		"password":   {"sepl"},
		"client_id":  {"admin-cli"},
		"grant_type": {"password"},
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := ioutil.ReadAll(resp.Body)
		err = errors.New(resp.Status + ": " + string(b))
		return
	}
	result := map[string]interface{}{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	token, _ = result["access_token"].(string)
	token = "Bearer " + token
	return
}
