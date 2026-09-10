BINARY := tama-link
BUILD_DIR := bin
VERSION ?= dev
COMMIT ?= unknown
BUILD_DATE ?= unknown
LDFLAGS := -s -w \
	-X github.com/kritama/tama-link/internal/version.Version=$(VERSION) \
	-X github.com/kritama/tama-link/internal/version.Commit=$(COMMIT) \
	-X github.com/kritama/tama-link/internal/version.BuildDate=$(BUILD_DATE)

.PHONY: build check clean fmt fmt-check lint race test vet

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/tama-link

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

check: fmt-check test race vet lint build

clean:
	rm -rf $(BUILD_DIR)
