FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o main .

FROM alpine:latest

RUN apk add --no-cache ca-certificates
COPY --from=builder /app/main /main

EXPOSE 3621

CMD ["/main"]