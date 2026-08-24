# Controller (design doc §4.2). Talks to Redis and to the Image Builder /
# Host Agent over the network — no special host privileges of any kind: no
# device access, no host networking, no mount capability, no Docker socket.
# A completely ordinary container, same shape as apiservice.Dockerfile.
#
# It used to run the Image Builder in-process, which needed CAP_SYS_ADMIN,
# loop-device access, and the host's Docker socket — genuinely unsafe to
# grant a container sharing the host's /dev (mount(2) on a loop device
# inside this container corrupted the container's own root filesystem more
# than once, independent of the separate AppArmor `deny mount,` issue that
# had to be worked around first just to get that far). The Image Builder now
# runs as its own systemd-managed process (cmd/imagebuilder) instead.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/controller ./cmd/controller
COPY internal/common ./internal/common
COPY internal/config ./internal/config
COPY internal/controller ./internal/controller
COPY internal/logging ./internal/logging
RUN CGO_ENABLED=0 go build -o /out/controller ./cmd/controller

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/controller /usr/local/bin/controller
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/controller"]
