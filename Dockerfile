FROM alpine:3.23 AS runtime
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 kioku && \
    mkdir /data && chown kioku:kioku /data
USER kioku
ENV KIOKU_ADDR=0.0.0.0:8080 KIOKU_DATA=/data
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["kiokud"]

FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Keep metadata changes from invalidating dependency downloads.
# Defaults support small self-hosted builders; CI can use the runner's resources.
ARG BUILD_PARALLELISM=1
ARG BUILD_MEMORY_LIMIT=768MiB
ARG BUILD_GOGC=50
ARG VERSION=dev
ARG COMMIT=unknown
RUN LDFLAGS="-X github.com/mash/kioku/internal/buildinfo.Version=$VERSION -X github.com/mash/kioku/internal/buildinfo.Commit=$COMMIT" && \
    GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 GOMEMLIMIT=$BUILD_MEMORY_LIMIT GOGC=$BUILD_GOGC go build -p $BUILD_PARALLELISM -trimpath -ldflags "$LDFLAGS" -o /out/kiokud ./cmd/kiokud && \
    GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 GOMEMLIMIT=$BUILD_MEMORY_LIMIT GOGC=$BUILD_GOGC go build -p $BUILD_PARALLELISM -trimpath -ldflags "$LDFLAGS" -o /out/kioku ./cmd/kioku

# The prebuilt target packages binaries cross-compiled elsewhere, so the host does not build Go.
FROM runtime AS prebuilt
COPY production-bin/kiokud production-bin/kioku /usr/local/bin/

FROM runtime AS final
COPY --from=build /out/kiokud /out/kioku /usr/local/bin/
