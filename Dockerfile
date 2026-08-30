# Static binary in an empty image: the pod mounts the config and the
# recording directory; nothing else is needed.
# Builder pinned by digest (golang:1.26, resolved 2026-08-29): the
# builder image controls the output binary, so pin it like a dependency.
FROM golang:1.26@sha256:dc2521c2a906db43073b8b4d99f491b6341cf15610b6ebbab187c45153f9959e AS build
ENV GOFLAGS=-mod=readonly
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /curtilage ./cmd/curtilage

FROM scratch
COPY --from=build /curtilage /curtilage
# curtilage runs as an unprivileged user; the manifest sets the uid.
ENTRYPOINT ["/curtilage"]
