# Static binary in an empty image: the pod mounts the config and the
# recording directory; nothing else is needed.
# Builder pinned by digest (golang:1.26, resolved 2026-08-29): the
# builder image controls the output binary, so pin it like a dependency.
FROM golang:1.26@sha256:dc2521c2a906db43073b8b4d99f491b6341cf15610b6ebbab187c45153f9959e AS build
ENV GOFLAGS=-mod=readonly
ARG VERSION=dev
ARG PRNUM=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.prnum=${PRNUM} -X main.built=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o /curtilage ./cmd/curtilage

FROM scratch
COPY --from=build /curtilage /curtilage
# ffmpeg, fully static, for the stitched follow view (server/stitch.go);
# a single-binary build that runs in an empty image.  Pinned by digest
# like every other input (mwader/static-ffmpeg:7.1.1, resolved
# 2026-09-07).
COPY --from=mwader/static-ffmpeg:7.1.1@sha256:11a44711684c0b9f754c047dcd64235b8b52deab251bd0e0a86f22faa160749c /ffmpeg /ffmpeg
# curtilage runs as an unprivileged user; the manifest sets the uid.
ENTRYPOINT ["/curtilage"]
