package moses

import (
	"bytes"
	"log"
	"net/http"
)

func Logger(handler http.Handler, logLevel string) *LoggerMiddleWare {
	return &LoggerMiddleWare{handler: handler, logLevel: logLevel}
}

type LoggerMiddleWare struct {
	handler  http.Handler
	logLevel string `DEBUG | CALL | NONE`
}

func (this *LoggerMiddleWare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	this.log(r)
	if this.handler != nil {
		this.handler.ServeHTTP(w, r)
	} else {
		http.Error(w, "Forbidden", 403)
	}
}

func (this *LoggerMiddleWare) log(request *http.Request) {
	if this.logLevel != "NONE" {
		method := request.Method
		path := request.URL

		if this.logLevel == "CALL" {
			log.Printf("[%v] %v \n", method, path)
		}

		if this.logLevel == "DEBUG" {
			buf := new(bytes.Buffer)
			buf.ReadFrom(request.Body)
			body := buf.String()

			client := request.RemoteAddr

			log.Printf("%v [%v] %v\n%v\n", client, method, path, body)
		}

	}
}
