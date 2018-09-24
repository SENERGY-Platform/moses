FROM golang:1.11


COPY . /go/src/moses
WORKDIR /go/src/moses

ENV GO111MODULE=on

RUN go build

EXPOSE 8080

CMD ./moses