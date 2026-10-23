FROM golang:1.27.3-alpine3.23 AS builder
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /app
RUN apk add --no-cache 'ca-certificates>=20261020-r0'
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.version=${VERSION#v}" -o /out/ktms-server ./cmd/server

FROM alpine:3.23 AS runtime
RUN apk add --no-cache 'ca-certificates>=20261020-r0' && adduser -D -u 10001 appuser

FROM runtime AS audio-runtime
RUN apk add --no-cache ffmpeg

# audio target includes ffmpeg for Gemini speech synthesis raw PCM transcoding (~172 MB)
FROM audio-runtime AS audio
WORKDIR /
COPY --from=builder /out/ktms-server /ktms-server
USER appuser
EXPOSE 8080 9090
ENTRYPOINT ["/ktms-server"]

# slim is the default target: kept as final stage for standard 'docker build .' default (~64 MB)
FROM runtime AS slim
WORKDIR /
COPY --from=builder /out/ktms-server /ktms-server
USER appuser
EXPOSE 8080 9090
ENTRYPOINT ["/ktms-server"]
