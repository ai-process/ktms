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

# audio target includes ffmpeg for Gemini speech synthesis raw PCM transcoding (~166 MB)
FROM alpine:3.21 AS audio
RUN apk add --no-cache ca-certificates ffmpeg && adduser -D -u 10001 appuser
WORKDIR /
COPY --from=builder /out/llm-proxy-server /llm-proxy-server
USER appuser
EXPOSE 8080 9090
ENTRYPOINT ["/llm-proxy-server"]

# slim is the default target: clean Alpine without ffmpeg for minimal image size (~65 MB)
FROM alpine:3.21 AS slim
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 appuser
WORKDIR /
COPY --from=builder /out/llm-proxy-server /llm-proxy-server
USER appuser
EXPOSE 8080 9090
ENTRYPOINT ["/llm-proxy-server"]
