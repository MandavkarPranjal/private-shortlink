# Build stage. The Go minor version and the CGO setting match .mise.toml, so
# the image and a local `mise run build` produce the same static binary.
FROM golang:1.27-alpine AS build

# git resolves module paths that need fetching; ca-certificates is needed for the
# downloads themselves.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Dependencies first: this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY doc.go ./
COPY cmd/ cmd/
COPY internal/ internal/

# The version is stamped at link time. .git is deliberately not in the build
# context (.dockerignore), so the commit cannot come from vcs.revision in the
# build info: pass it in instead. `mise run docker:build` does that with
# `git describe --tags --always --long`. A bare `docker build` leaves VERSION
# empty and the binary reports itself as an unstamped development build.
ARG VERSION=
RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags "-s -w${VERSION:+ -X main.version=$VERSION}" \
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

# Working directory is a volume, so a relative -db or -key-file default (the
# binary falls back to ./shortlink.db and ./shortlink-tailcat.key) still lands in
# persistent storage. Without this, an overridden CMD that leaves out -db would
# try to create the database in /, which uid 65532 cannot write to.
WORKDIR /data

EXPOSE 8080
USER nonroot:nonroot

# Defaults suit local mode, which binds a high port because nonroot cannot bind
# port 80. Overriding CMD replaces the whole list, so a mesh-mode run has to name
# the paths it wants to keep persistent:
#   docker run -p 8080:8080 -v shortlink-data:/data shortlink:latest
#   docker run --network host -e TS_AUTHKEY=tskey-auth-... \
#     -v shortlink-data:/data -v shortlink-config:/config shortlink:latest \
#     -mode tailnet -db /data/shortlink.db -state-dir /config/shortlink-tsnet
# tsnet embeds a userspace network stack, so it needs no TUN device and no
# capabilities. netbird mode binds a port on the host's mesh interface, which
# needs host networking and, for port 80, --cap-add NET_BIND_SERVICE.
ENTRYPOINT ["/usr/local/bin/shortlink"]
CMD ["-mode", "local", "-listen", ":8080", "-db", "/data/shortlink.db", "-state-dir", "/config/shortlink-tsnet", "-key-file", "/data/shortlink-tailcat.key"]