# The image of this fork's build. Its context is the source build.sh leaves in <out-dir>/src:
#
#   docker buildx build --platform linux/amd64,linux/arm64 -f Dockerfile out/src
#
# The layout inside is upstream's (eceasy/cli-proxy-api), so a deployment of upstream's image
# takes this one unchanged: the program is /CLIProxyAPI/CLIProxyAPI, started from /CLIProxyAPI.
# The program is compiled on the build machine for both platforms, not under emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS builder

RUN apt-get update \
  && apt-get install -y --no-install-recommends build-essential gcc-aarch64-linux-gnu libc6-dev-arm64-cross gcc-x86-64-linux-gnu libc6-dev-amd64-cross \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

# The plugin loader needs cgo, so each platform gets its own C compiler.
RUN case "$TARGETARCH" in \
      amd64) CC=x86_64-linux-gnu-gcc ;; \
      arm64) CC=aarch64-linux-gnu-gcc ;; \
      *) echo "no C compiler set up for $TARGETARCH" >&2; exit 1 ;; \
    esac \
  && CC=$CC CGO_ENABLED=1 GOOS=linux GOARCH=$TARGETARCH go build -buildvcs=false \
      -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" \
      -o /out/CLIProxyAPI ./cmd/server

FROM debian:bookworm-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends tzdata ca-certificates \
  && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI
COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI
EXPOSE 8317
CMD ["./CLIProxyAPI"]
