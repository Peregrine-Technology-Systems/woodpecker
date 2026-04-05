# Multi-stage Dockerfile for Peregrine Woodpecker fork
# Builds the full server (frontend + backend + plugins) from source
#
# Stage 1: Build Vue frontend with pnpm
# Stage 2: Build Go server binary (embeds frontend via go:embed)
# Stage 3: Minimal scratch runtime

# ── Stage 1: Frontend ─────────────────────────────────────────────
FROM docker.io/node:22-alpine AS frontend

WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml ./
RUN corepack enable && corepack prepare pnpm@latest --activate && pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

# ── Stage 2: Go build ────────────────────────────────────────────
FROM docker.io/golang:1.26 AS build

ARG VERSION=dev

RUN groupadd -g 1000 woodpecker && \
  useradd -u 1000 -g 1000 woodpecker && \
  mkdir -p /var/lib/woodpecker

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=frontend /src/web/dist web/dist/

# CGO_ENABLED=1 required for SQLite (mattn/go-sqlite3)
# Static link so binary works on Alpine/scratch (musl vs glibc)
RUN CGO_ENABLED=1 go build \
  -ldflags "-s -w -extldflags '-static' -X go.woodpecker-ci.org/woodpecker/v3/version.Version=${VERSION}" \
  -o /build/woodpecker-server \
  ./cmd/server

# Agent does not need CGO (no SQLite)
RUN CGO_ENABLED=0 go build \
  -ldflags "-s -w -X go.woodpecker-ci.org/woodpecker/v3/version.Version=${VERSION}" \
  -o /build/woodpecker-agent \
  ./cmd/agent

# ── Stage 3: Runtime ─────────────────────────────────────────────
# Use Alpine instead of scratch — CGO binary needs libc
FROM docker.io/alpine:3.22

RUN apk add --no-cache ca-certificates && \
  addgroup -g 1000 woodpecker && \
  adduser -u 1000 -G woodpecker -D woodpecker && \
  mkdir -p /var/lib/woodpecker && \
  chown woodpecker:woodpecker /var/lib/woodpecker

ENV GODEBUG=netdns=go
ENV WOODPECKER_IN_CONTAINER=true
ENV XDG_CACHE_HOME=/var/lib/woodpecker
ENV XDG_DATA_HOME=/var/lib/woodpecker
EXPOSE 8000 9000 80 443

COPY --from=build /build/woodpecker-server /bin/woodpecker-server

USER woodpecker

HEALTHCHECK CMD ["/bin/woodpecker-server", "ping"]
ENTRYPOINT ["/bin/woodpecker-server"]
