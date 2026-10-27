# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.23-bookworm AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /vigil ./cmd/vigil

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates tzdata util-linux \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /vigil /usr/local/bin/vigil
COPY docker/entrypoint.sh /entrypoint.sh
ENV PUID=1000 PGID=1000 VIGIL_CONFIG=/config VIGIL_RECORDINGS=/recordings VIGIL_BUFFER=/tmp/vigil VIGIL_PORT=8080 \
    GOMEMLIMIT=160MiB GOGC=50
VOLUME ["/config", "/recordings"]
EXPOSE 8080
ENTRYPOINT ["/entrypoint.sh"]
