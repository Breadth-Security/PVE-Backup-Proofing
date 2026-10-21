# syntax=docker/dockerfile:1
# Pinned to the `toolchain` in go.mod (the test workflow checks they match).
FROM golang:1.26.10 AS build
# The version stamped into the binary: --build-arg VERSION=... when given,
# otherwise the top-level VERSION file, so a plain `docker build` or
# `docker compose build` stamps a release number the node helper accepts
# (a "dev" daemon is compatible only with a "dev" helper).
ARG VERSION=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Refuse to build an image from code that fails vet or the race-enabled
# tests (the race detector needs cgo, which the golang image provides).
RUN go vet ./... && go vet -tags integration ./... && CGO_ENABLED=1 go test -race -count=1 ./...
RUN v="${VERSION:-$(packaging/versionfile.sh)}" \
    && echo "stamping version $v" \
    && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$v" -o /out/pbp ./cmd/pbp
# Empty state and log directories, copied below with uid 65532 ownership so
# named volumes mounted there inherit it.
RUN mkdir -p /out/var/lib/pbp/state /out/var/log/pbp

FROM gcr.io/distroless/static-debian12:nonroot
# On the PATH, so `docker compose exec pbp pbp …` finds it (code review
# 6.13: /pbp was not, and the documented commands failed).
COPY --from=build /out/pbp /usr/local/bin/pbp
COPY --from=build --chown=65532:65532 /out/var/lib/pbp /var/lib/pbp
COPY --from=build --chown=65532:65532 /out/var/log/pbp /var/log/pbp
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/pbp"]
CMD ["daemon"]
HEALTHCHECK --interval=5m --timeout=20s CMD ["/usr/local/bin/pbp","health"]
