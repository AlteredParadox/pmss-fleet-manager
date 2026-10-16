BINARY := pmss-fleet-manager
# Stamped into the binary as main.version. The release workflow overrides
# this with the tag; local builds report the git description, or "dev".
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build run test vet clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/$(BINARY)

run:
	go run ./cmd/$(BINARY)

test:
	go test ./...

vet:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...

clean:
	rm -f $(BINARY)
