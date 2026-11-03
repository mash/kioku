FROM alpine:3.23 AS runtime
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 kioku && \
    mkdir /data && chown kioku:kioku /data
USER kioku
ENV KIOKU_ADDR=0.0.0.0:8080 KIOKU_DATA=/data
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["kiokud"]

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN LDFLAGS="-X github.com/mash/kioku/internal/buildinfo.Version=$VERSION -X github.com/mash/kioku/internal/buildinfo.Commit=$COMMIT" && \
    GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 GOMEMLIMIT=768MiB GOGC=50 go build -p 1 -trimpath -ldflags "$LDFLAGS" -o /out/kiokud ./cmd/kiokud && \
    GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 GOMEMLIMIT=768MiB GOGC=50 go build -p 1 -trimpath -ldflags "$LDFLAGS" -o /out/kioku ./cmd/kioku

# The prebuilt target packages binaries cross-compiled elsewhere, so the host does not build Go.
FROM runtime AS prebuilt
COPY production-bin/kiokud production-bin/kioku /usr/local/bin/

FROM runtime AS final
COPY --from=build /out/kiokud /out/kioku /usr/local/bin/
