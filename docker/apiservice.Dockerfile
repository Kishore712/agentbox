# REST API Service (design doc §4.1) — the one service with no special host
# privileges of any kind: no device access, no host networking, no mount
# capability, no Docker socket. A completely ordinary container.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/apiservice ./cmd/apiservice
COPY internal/apiservice ./internal/apiservice
COPY internal/config ./internal/config
COPY internal/logging ./internal/logging
RUN CGO_ENABLED=0 go build -o /out/apiservice ./cmd/apiservice

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/apiservice /usr/local/bin/apiservice
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/apiservice"]
