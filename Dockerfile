# syntax=docker/dockerfile:1.7
#
# Builds the hcloud-talos-cluster block operator image.
# Usage: DOCKER_BUILDKIT=1 docker build .
# The operator-kit and talos-cluster-kit modules are public, so go mod download
# fetches them through the Go module proxy with no credential.

FROM golang:1.26.10-alpine3.24@sha256:9ce231b28998b32e116927c61aa5c406183061d4f3f2d61b292c88e7f6bdf7ca AS build
RUN apk add --no-cache git
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -ldflags="-s -w" -o /out/block .

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
# The upgrade takes the fixes Alpine published after the pinned base was built,
# which the publish job's scan refuses the image without.
RUN apk upgrade --no-cache && adduser -D -u 10001 block
WORKDIR /app
COPY --from=build /out/block /app/block
# kube-app-deploy runs a pod as a numeric uid on a read-only root with only /tmp writable.
ENV HOME=/tmp
USER 10001:10001
# The hosting compose sets LISTEN_ADDR to the port its Caddy route names.
EXPOSE 8080
ENTRYPOINT ["/app/block"]
