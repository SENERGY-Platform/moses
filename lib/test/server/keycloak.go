package server

import (
	"encoding/json"
	"github.com/ory/dockertest"
	"log"
	"net/http"
	"net/url"
	"strings"
)

func Keycloak(pool *dockertest.Pool) (closer func(), hostPort string, ipAddress string, err error) {
	log.Println("start keycloak")
	keycloak, err := pool.Run("fgseitsrancher.wifa.intern.uni-leipzig.de:5000/testkeycloak", "test", []string{
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
