FROM golang:1.23-alpine AS builder
RUN apk add --no-cache git
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/api ./cmd/api
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o api ./cmd/api

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /build/api /usr/local/bin/api
EXPOSE 8080
ENTRYPOINT ["api"]
