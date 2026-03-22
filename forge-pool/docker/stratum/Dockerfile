FROM golang:1.23-alpine AS builder
RUN apk add --no-cache git
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/stratum ./cmd/stratum
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o stratum ./cmd/stratum

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /build/stratum /usr/local/bin/stratum
EXPOSE 3333
ENTRYPOINT ["stratum"]
