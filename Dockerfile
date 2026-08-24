# CSS is compiled from Tailwind at image-build time so a forgotten
# `npm run css` cannot ship stale styles. The runtime image stays a
# static Go binary.
FROM node:22-alpine AS css
WORKDIR /web
COPY internal/web/package.json internal/web/package-lock.json ./
RUN npm ci
COPY internal/web/styles ./styles
COPY internal/web/templates ./templates
COPY internal/web/web.go ./web.go
RUN npm run css

# Build: pure Go, so the binary is static and the runtime image needs no libc
# shim. modernc.org/sqlite is what makes CGO_ENABLED=0 possible.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=css /web/static/app.css internal/web/static/app.css
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/coolify-github-ci ./cmd/server

# Runtime: git is required to check out commits, and Node is the default
# pipeline runtime (PLAN.md). Everything runs as a non-root user.
FROM alpine:3.21
RUN apk add --no-cache git nodejs npm ca-certificates tini \
 && addgroup -g 10001 ci \
 && adduser -D -u 10001 -G ci -h /home/ci ci \
 && mkdir -p /data /workspace \
 && chown -R ci:ci /data /workspace /home/ci

COPY --from=build /out/coolify-github-ci /usr/local/bin/coolify-github-ci

USER ci
WORKDIR /home/ci
ENV DATA_DIR=/data \
    WORKSPACE_DIR=/workspace \
    LISTEN_ADDR=:8080
EXPOSE 8080
VOLUME ["/data", "/workspace"]

# tini reaps the shells a pipeline step leaves behind.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/coolify-github-ci"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/health >/dev/null || exit 1
