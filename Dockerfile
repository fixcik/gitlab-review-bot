# Service image: `reviewbot serve` and `reviewbot egress`.
#
#   docker build -t reviewbot:latest .

# Go 1.27 (current; 1.26 is the other supported release). go.mod keeps
# `go 1.24.7` as the minimum language version.
ARG GO_VERSION=1.27
ARG DOCKER_CLI_VERSION=27

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# cgo is required by github.com/mattn/go-sqlite3.
ENV CGO_ENABLED=1
# VERSION ends up in reviewbot_build_info (docker build --build-arg VERSION=1.2.3).
ARG VERSION=dev
RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/reviewbot ./cmd/reviewbot

FROM docker:${DOCKER_CLI_VERSION}-cli AS dockercli

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git tini \
 && rm -rf /var/lib/apt/lists/*
# The bot drives sandbox containers through the docker CLI (the socket is
# mounted at runtime; see docker-compose.yml).
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=build /out/reviewbot /usr/local/bin/reviewbot

RUN useradd --uid 1000 --create-home --shell /usr/sbin/nologin reviewbot
USER 1000:1000
ENV HOME=/home/reviewbot
EXPOSE 8080
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["reviewbot", "serve", "-config", "/etc/reviewbot/config.yaml"]
