# ReviewBench agent image: `reviewbot bench` reviews the pull request mounted
# at /work (https://github.com/review-bench/ReviewBench, AGENT_CONTRACT.md).
#
#   docker build --platform linux/amd64 -f bench/Dockerfile -t reviewbot-bench:dev .
#
# Credential: ANTHROPIC_API_KEY (or CLAUDE_CODE_OAUTH_TOKEN), passed at run time.
# Egress: api.anthropic.com. Configuration: RB_CONFIG_MODEL (claude --model).
ARG GO_VERSION=1.27
FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=1
RUN go build -trimpath -ldflags="-s -w" -o /out/reviewbot ./cmd/reviewbot

FROM node:22-bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git ripgrep \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g @anthropic-ai/claude-code \
 && npm cache clean --force
COPY --from=build /out/reviewbot /usr/local/bin/reviewbot
ENV DISABLE_AUTOUPDATER=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1 USE_BUILTIN_RIPGREP=0
USER node
WORKDIR /work/repo
ENTRYPOINT ["reviewbot", "bench"]
