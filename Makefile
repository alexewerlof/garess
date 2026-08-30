BINARY := garess
DIST   := dist

.PHONY: all build build-arm test vet fmt run doctor clean

all: build

build:
	go build -trimpath -o $(DIST)/$(BINARY) ./cmd/garess

# Raspberry Pi 1 (ARMv6, 32-bit) cross-compile.
# GOARM=6 is required: Go 1.21+ defaults cross-builds to GOARM=7, and ARMv5
# support was dropped entirely.
build-arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 \
		go build -trimpath -ldflags="-s -w" -o $(DIST)/$(BINARY)-linux-armv6 ./cmd/garess

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
