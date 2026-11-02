# Cross-compiles on the build machine, so multi-arch images need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
COPY . /workspace
WORKDIR /workspace
ENV CGO_ENABLED=0
ARG VERSION
ARG TARGETOS
ARG TARGETARCH
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
  -trimpath \
  -ldflags "\
    -s -w \
    -X main.version=${VERSION}" \
  -o /workspace/go-pmtiles
FROM gcr.io/distroless/static
COPY --from=builder /workspace/go-pmtiles /go-pmtiles
EXPOSE 8080
ENTRYPOINT ["/go-pmtiles"]
