# syntax=docker/dockerfile:1

# --- Build stage -----------------------------------------------------------
# garess is pure Go and statically linked (CGO_ENABLED=0), so the build stage
# needs nothing but the Go toolchain. The image is built for multiple
# platforms (linux/amd64, linux/arm64, linux/arm/v7) by .github/workflows/
# release.yml on every v* tag push; VERSION is stamped like the binary assets.
FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/garess ./cmd/garess

# --- Runtime stage ---------------------------------------------------------
# Alpine (not scratch/distroless) on purpose: hooks run via `sh -c` and the
# bash tool runs `bash -lc`, so a POSIX shell and bash must exist at runtime.
# The container is the confinement boundary, so the Landlock sandbox stays
# off (the config default backend is "none" — do not enable landlock inside a
# container). Run with `docker run -it` — the TUI needs a TTY.
FROM alpine:3.22
ARG VERSION=dev
RUN apk add --no-cache bash ca-certificates \
    && adduser -D -h /home/garess garess
COPY --from=build /out/garess /usr/local/bin/garess
ENV VERSION=${VERSION}
USER garess
WORKDIR /work
ENTRYPOINT ["garess"]
