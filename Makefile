BINARY := garess
DIST   := dist
# Version stamped into the binary (garess --version). Pass VERSION=vX.Y.Z for
# release builds; plain `make build` reports "dev".
#
# -X main.version, NOT garess/cmd/garess.version: the Go compiler names
# symbols in the main package "main.<name>" regardless of the package's
# import path (here cmd/garess), so -X against the full path silently no-ops.
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

# Every release asset built locally (all static, CGO_ENABLED=0). CI
# (goreleaser on a v* tag) is the real release path; this target is for
# pre-push smoke tests and a manual fallback.
RELEASE_TARGETS := \
	linux-amd64 linux-arm64 linux-armv6 \
	darwin-amd64 darwin-arm64 \
	windows-amd64 windows-arm64 \
	freebsd-amd64 freebsd-arm64

.PHONY: all build build-all build-arm test vet fmt run doctor clean

all: build

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY) ./cmd/garess

# Build every release asset into dist/. Suffixes match the GitHub release
# naming (linux-armv6, windows-amd64.exe, ...).
#
# env(1) is used (not a bare CGO_ENABLED=0 ... prefix) because bash only
# treats LITERAL NAME=value tokens as env prefixes — the GOARM=6 arriving via
# $$extra is an expansion, so it would be parsed as a command instead. Do NOT
# add # comments inside this backslash-continued recipe; they swallow the rest
# of the logical line.
build-all:
	@set -e; for t in $(RELEASE_TARGETS); do \
		os=$${t%%-*}; arch=$${t##*-}; extra=""; name="$(DIST)/$(BINARY)-$$t"; \
		case $$t in \
			linux-armv6) arch=arm; extra=GOARM=6 ;; \
			windows-*) name="$$name.exe" ;; \
		esac; \
		echo "==> $$t"; \
		env CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $$extra \
			go build -trimpath -ldflags="$(LDFLAGS)" -o $$name ./cmd/garess; \
	done

# Raspberry Pi 1 (ARMv6, 32-bit) cross-compile.
# GOARM=6 is required: Go 1.21+ defaults cross-builds to GOARM=7, and ARMv5
# support was dropped entirely.
build-arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 \
		go build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-armv6 ./cmd/garess

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

run:
	go run ./cmd/garess

doctor:
	go run ./cmd/garess doctor

clean:
	rm -rf $(DIST)
