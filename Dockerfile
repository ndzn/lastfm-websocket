FROM golang:1.20-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o main .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates && \
    adduser -D -u 10001 appuser
COPY --from=builder /app/main /main

USER appuser
EXPOSE 3621

CMD ["/main"]