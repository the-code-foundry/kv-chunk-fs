# Example only: kv-chunk-fs in a container. The host service
# (deploy/kv-chunk-fs.service) needs no extra privileges; see README.md
# in this folder for what a container needs instead.
#
#   docker build -f deploy/docker/Dockerfile -t kv-chunk-fs .     (from the repo root)
FROM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /kv-chunk-fs ./cmd/kv-chunk-fs

FROM debian:trixie-slim
# fusermount3 for the mount; the binary itself is static
RUN apt-get update && apt-get install -y --no-install-recommends fuse3 \
 && rm -rf /var/lib/apt/lists/* \
 && sed -i 's/^#user_allow_other/user_allow_other/' /etc/fuse.conf
COPY --from=build /kv-chunk-fs /usr/local/bin/kv-chunk-fs
# health: the first line of the status file inside the mount
HEALTHCHECK --interval=30s --timeout=5s \
  CMD [ "$(head -1 /kv/cache/status)" = healthy ]
ENTRYPOINT ["kv-chunk-fs", "mount", "--backing", "/backing", "/kv/cache"]
