FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /mock-upstream main.go

FROM alpine:3.21
COPY --from=builder /mock-upstream /mock-upstream
USER 10001
EXPOSE 8000
ENTRYPOINT ["/mock-upstream"]
