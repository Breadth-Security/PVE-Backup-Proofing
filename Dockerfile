# syntax=docker/dockerfile:1
FROM golang:1.26 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/pbp ./cmd/pbp
# Empty state and log directories, copied below with uid 65532 ownership so
# named volumes mounted there inherit it.
RUN mkdir -p /out/var/lib/pbp /out/var/log/pbp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/pbp /pbp
COPY --from=build --chown=65532:65532 /out/var/lib/pbp /var/lib/pbp
COPY --from=build --chown=65532:65532 /out/var/log/pbp /var/log/pbp
USER 65532:65532
ENTRYPOINT ["/pbp"]
CMD ["daemon"]
HEALTHCHECK --interval=5m --timeout=20s CMD ["/pbp","health"]
