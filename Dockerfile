# Build stage
# Pinned patch release: includes the stdlib security fixes govulncheck flags
# (kept in sync with go.mod `toolchain` and .github/workflows/ci.yml GO_VERSION).
FROM golang:1.26.5 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO is off: modernc.org/sqlite is pure Go, so the binary is static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/postra ./cmd/postra

# Runtime stage
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/postra /usr/local/bin/postra
# Data (SQLite, object store, local secret store, KEK) lives here; mount a
# volume so it survives restarts. In server mode, inject POSTRA_KEK from
# Vault/OpenBao instead of relying on the on-disk KEK file.
ENV POSTRA_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8480 8481
USER nonroot:nonroot
ENTRYPOINT ["postra"]
CMD ["serve"]
