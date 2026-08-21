FROM golang:1.26 AS builder

COPY . /go/src/app
WORKDIR /go/src/app

ENV GO111MODULE=on

# the openapi specification is generated here rather than committed, so it can
# never drift from the annotations it is generated from
RUN go generate ./...

RUN CGO_ENABLED=0 GOOS=linux go build -o app

RUN git log -1 --oneline > version.txt

FROM alpine:latest
WORKDIR /root/
COPY --from=builder /go/src/app/app .
COPY --from=builder /go/src/app/config.json .
COPY --from=builder /go/src/app/version.txt .
COPY --from=builder /go/src/app/docs ./docs

EXPOSE 8080

ENTRYPOINT ["./app"]
