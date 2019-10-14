package helper

import (
	"bytes"
	"encoding/json"
	"github.com/SENERGY-Platform/moses/lib/test/server"
	"net/http"
	"runtime/debug"
	"testing"
)

func AdminGet(t *testing.T, url string, result interface{}) (err error) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		debug.PrintStack()
		return err
	}
	req.Header.Set("Authorization", string(server.AdminJwt))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		debug.PrintStack()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		debug.PrintStack()
		return err
	}
	err = json.NewDecoder(resp.Body).Decode(&result)
	return err
}
