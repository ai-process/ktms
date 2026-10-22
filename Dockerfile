FROM golang:1.27.3-alpine3.23 AS builder
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /app
RUN apk add --no-cache 'ca-certificates>=20261020-r0'
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/llm-proxy-server ./cmd/server

FROM alpine:3.23
# ffmpeg is required by the speech path: Gemini TTS returns raw PCM, which is
# transcoded to OGG/Opus before it goes back over the wire.
RUN apk add --no-cache 'ca-certificates>=20261020-r0' ffmpeg && adduser -D -u 10001 appuser
WORKDIR /
COPY --from=builder /out/llm-proxy-server /llm-proxy-server
USER appuser
EXPOSE 8080 9090
ENTRYPOINT ["/llm-proxy-server"]
