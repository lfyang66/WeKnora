# Build the paired extension and fetch the checksum-pinned native daemon.
# Node runs on the builder architecture; only bsk targets the runtime image.
# [local-build] node:24-bookworm (non-slim) ships git/python3/ca-certificates; apt-get skipped (GPG broken in this env)
FROM --platform=$BUILDPLATFORM node:24-bookworm AS browserskill
WORKDIR /build
COPY scripts/build_browserskill.sh scripts/browserskill-release.json ./scripts/
COPY patches/browserskill ./patches/browserskill
ARG TARGETOS
ARG TARGETARCH
RUN bash scripts/build_browserskill.sh /opt/weknora/browserskill "${TARGETOS}/${TARGETARCH}"

# Build stage
FROM golang:1.26-bookworm AS builder

WORKDIR /app

# 通过构建参数接收敏感信息
ARG GOPRIVATE_ARG
ARG GOPROXY_ARG
ARG GOSUMDB_ARG=off
ARG APK_MIRROR_ARG

# 设置Go环境变量
ENV GOPRIVATE=${GOPRIVATE_ARG}
ENV GOPROXY=${GOPROXY_ARG}
ENV GOSUMDB=${GOSUMDB_ARG}

# Install dependencies
RUN echo "[local-build] skip apt-get: golang image ships git/gcc/make/curl; libsqlite3-dev handled below if needed"

# Install migrate tool
RUN go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest

# Copy go mod files. go.mod replace-points anydoc at ./third_party/anydoc-go,
# so that module's go.mod must exist before `go mod download`.
COPY go.mod go.sum ./
COPY third_party/anydoc-go/go.mod third_party/anydoc-go/go.mod
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/download cmd/download
RUN go run cmd/download/duckdb/duckdb.go
COPY . .
RUN bash ./scripts/check-license-bundle.sh

# Get version and commit info for build injection
ARG VERSION_ARG
ARG COMMIT_ID_ARG
ARG BUILD_TIME_ARG
ARG GO_VERSION_ARG

# Set build-time variables
ENV VERSION=${VERSION_ARG}
ENV COMMIT_ID=${COMMIT_ID_ARG}
ENV BUILD_TIME=${BUILD_TIME_ARG}
ENV GO_VERSION=${GO_VERSION_ARG}

# Link the anydoc parser engine (office docs converted in-process, no
# Python docreader). Default on so Hub / compose images ship a working
# engine; pass WITH_ANYDOC=0 to skip the Rust toolchain (~few minutes and
# ~1 GB of build-stage layers).
ARG WITH_ANYDOC=1
ENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo
ENV PATH=/usr/local/cargo/bin:$PATH
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/usr/local/cargo/git \
    if [ "$WITH_ANYDOC" = "1" ]; then \
        mkdir -p "$CARGO_HOME" && \
        printf '[source.crates-io]\nreplace-with = "rsproxy-sparse"\n[source.rsproxy-sparse]\nregistry = "sparse+https://rsproxy.cn/index/"\n' > "$CARGO_HOME/config.toml" && \
        curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
            | RUSTUP_DIST_SERVER=https://rsproxy.cn RUSTUP_UPDATE_ROOT=https://rsproxy.cn/rustup sh -s -- -y --profile minimal --default-toolchain stable && \
        ./scripts/build-anydoc-lib.sh; \
    fi

# [local-build] provide sqlite3.h from mattn/go-sqlite3 amalgamation (libsqlite3-dev not installed)
RUN --mount=type=cache,target=/go/pkg/mod \
    find /go/pkg/mod/github.com/mattn -name "sqlite3-binding.h" 2>/dev/null | head -1 > /tmp/hdr; \
    if [ ! -s /tmp/hdr ]; then \
        echo "[local-build] mod cache empty, downloading mattn/go-sqlite3"; \
        go mod download github.com/mattn/go-sqlite3; \
        find /go/pkg/mod/github.com/mattn -name "sqlite3-binding.h" | head -1 > /tmp/hdr; \
    fi; \
    cp "$(cat /tmp/hdr)" /usr/include/sqlite3.h && ls -la /usr/include/sqlite3.h

# Build the application with version info
RUN --mount=type=cache,target=/go/pkg/mod \
    if [ "$WITH_ANYDOC" = "1" ]; then \
        make build-prod GO_BUILD_TAGS=anydoc; \
    else \
        make build-prod; \
    fi
RUN --mount=type=cache,target=/go/pkg/mod cp -r /go/pkg/mod/github.com/yanyiwu/ /app/yanyiwu/

# Final stage
FROM debian:12.12-slim

WORKDIR /app

ARG APK_MIRROR_ARG

# Pairing derives the gateway URL from the user's page origin by default.
ENV BROWSERSKILL_BINARY=/opt/weknora/browserskill/bsk \
    BROWSERSKILL_EXTENSION_PATH=/opt/weknora/browserskill/browser-skill-weknora-0.2.1.zip
COPY --from=browserskill /opt/weknora/browserskill /opt/weknora/browserskill

# Create a non-root user first
RUN useradd -m -s /bin/bash appuser

# First, install ca-certificates without mirror to ensure HTTPS works
# [local-build] CA bundle copied from builder (apt skipped: GPG broken in this env)
COPY --from=builder /etc/ssl/certs /etc/ssl/certs

# Then switch to mirror if specified and install other packages
# [local-build] sandbox toolchain (python3/node/ffmpeg/uv) skipped — minimal KB deployment; reinstall via apt if sandbox needed

# Create data directories and set permissions
RUN mkdir -p /data/files && \
    chown -R appuser:appuser /app /data/files

# Copy migrate tool from builder stage
COPY --from=builder /go/bin/migrate /usr/local/bin/
COPY --from=builder /app/yanyiwu/ /go/pkg/mod/github.com/yanyiwu/

# Copy the binary from the builder stage
COPY --from=builder /app/config ./config
COPY --from=builder /app/scripts ./scripts
COPY --from=builder /app/migrations ./migrations
COPY --from=builder /app/dataset/samples ./dataset/samples
COPY --from=builder /root/.duckdb /home/appuser/.duckdb
# [local-build 2026-09-16] root runtime (gosu removed) reads HOME=/root
COPY --from=builder /root/.duckdb /root/.duckdb
COPY --from=builder /app/WeKnora .
COPY LICENSE THIRD_PARTY_NOTICES.md ./
COPY licenses ./licenses

# Copy and make entrypoint script executable
COPY --from=builder /app/scripts/docker-entrypoint.sh ./scripts/docker-entrypoint.sh

# Make scripts executable
RUN chmod +x ./scripts/*.sh

# Expose ports
EXPOSE 8080


ENTRYPOINT ["./scripts/docker-entrypoint.sh"]
CMD ["./WeKnora"]
