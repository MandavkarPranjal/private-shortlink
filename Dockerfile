# Build stage. The Go minor version and the CGO setting match .mise.toml, so
# the image and a local `mise run build` produce the same static binary.
FROM golang:1.27-alpine AS build

# git resolves some module paths and lets the linker stamp vcs.revision;
# ca-certificates is needed for module downloads.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Dependencies first: this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY doc.go ./
COPY cmd/ cmd/
COPY internal/ internal/

# The version is stamped at link time; vcs.revision and vcs.modified are
# embedded automatically from the build context's .git directory.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/shortlink ./cmd/shortlink

# State directories with the ownership distroless' nonroot user (65532) needs.
# Creating them here means a fresh named volume inherits that ownership instead
# of being created root-owned and unwritable.
RUN mkdir -p /data /config && chown 65532:65532 /data /config

# Runtime stage: distroless static. No shell and no package manager, so there is
# nothing to exec into if the process is compromised. The binary is fully static
# (CGO_ENABLED=0), so the "static" variant is enough.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/shortlink /usr/local/bin/shortlink
COPY --from=build --chown=65532:65532 /data /data
COPY --from=build --chown=65532:65532 /config /config

# /data holds the SQLite database and its WAL sidecars; /config holds tsnet node
# state and the tailcat key file. Mount both to keep links and identities across
# container replacements.
VOLUME ["/data", "/config"]

EXPOSE 8080
USER nonroot:nonroot

# Defaults suit local mode, which binds a high port because nonroot cannot bind
# port 80. Override the whole CMD for the mesh modes, e.g.
#   docker run -p 8080:8080 shortlink:latest
#   docker run --network host --device /dev/net/tun --cap-add NET_ADMIN \
#     -e TS_AUTHKEY=tskey-auth-... shortlink:latest \
#     -mode tailnet -state-dir /config/shortlink-tsnet
ENTRYPOINT ["/usr/local/bin/shortlink"]
CMD ["-mode", "local", "-listen", ":8080", "-db", "/data/shortlink.db", "-state-dir", "/config/shortlink-tsnet", "-key-file", "/config/shortlink-tailcat.key"]