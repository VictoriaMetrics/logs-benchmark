# cross-compile on the build host, so a multi-arch release needs no emulation
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /bench ./cmd/bench

FROM alpine:3.24
COPY --from=builder /bench /bench
ENTRYPOINT ["/bench"]
