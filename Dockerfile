# Browser assets are rebuilt from the locked dependencies in the builder only.
# The final image contains the embedded Go binary, not Node or node_modules.
FROM node:22-bookworm-slim AS frontend
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
COPY assets/ /src/assets/
RUN npm run build

# Go build stage
# Pinned patch release: includes the stdlib security fixes govulncheck flags
# (kept in sync with go.mod `toolchain` and .github/workflows/ci.yml GO_VERSION).
FROM golang:1.26.6 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/internal/transport/spa/assets ./internal/transport/spa/assets
# Version is injected at build time; defaults to "dev" for plain docker builds.
ARG VERSION=dev
# CGO is off: modernc.org/sqlite is pure Go, so the binary is static.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X postra/internal/platform/build.Version=${VERSION}" \
    -o /out/postra ./cmd/postra

# Runtime stage
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/postra /usr/local/bin/postra
# Data (SQLite, object store, local secret store, KEK) lives here; mount a
# volume so it survives restarts. In server mode, inject POSTRA_KEK from
# Vault/OpenBao instead of relying on the on-disk KEK file.
ENV POSTRA_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8480
USER nonroot:nonroot
ENTRYPOINT ["postra"]
CMD ["serve"]
