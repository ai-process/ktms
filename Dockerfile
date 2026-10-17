FROM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /app
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/llm-proxy-server ./cmd/server

# base runtime stage shared by audio and slim variants
FROM alpine:3.21 AS base
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 appuser
WORKDIR /
COPY --from=builder /out/llm-proxy-server /llm-proxy-server
USER appuser
EXPOSE 8080 9090
ENTRYPOINT ["/llm-proxy-server"]

# audio target includes ffmpeg for Gemini speech synthesis raw PCM transcoding (~172 MB)
FROM base AS audio
USER root
RUN apk add --no-cache ffmpeg
USER appuser

# slim is the default target: kept as final stage for standard 'docker build .' default (~64 MB)
FROM base AS slim
