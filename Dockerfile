# syntax=docker/dockerfile:1

FROM golang:1.26.6-alpine3.24 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /lastfm-websocket .

FROM alpine:3.24

RUN apk add --no-cache ca-certificates && \
    adduser -D -u 10001 appuser
COPY --from=builder /lastfm-websocket /usr/local/bin/lastfm-websocket

USER appuser
EXPOSE 3621

CMD ["/usr/local/bin/lastfm-websocket"]
