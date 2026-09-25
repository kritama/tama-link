BINARY := tama-link
BUILD_DIR := bin
VERSION ?= dev
COMMIT ?= unknown
BUILD_DATE ?= unknown
LDFLAGS := -s -w \
	-X github.com/kritama/tama-link/internal/version.Version=$(VERSION) \
	-X github.com/kritama/tama-link/internal/version.Commit=$(COMMIT) \
	-X github.com/kritama/tama-link/internal/version.BuildDate=$(BUILD_DATE)

.PHONY: build check clean compose-accept fmt fmt-check lint live-accept race test vet

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

# compose-accept and live-accept are not part of check. compose-accept
# resolves the Compose model and checks every selectable image or build
# source. It does not start containers.
# live-accept is an unimplemented scaffold: it fails closed and does not
# exercise profiles. See wip/tama-link-specification/acceptance/phase-2.md.
compose-accept:
	go test -tags=compose -count=1 -timeout=5m ./internal/acceptance/compose

live-accept:
	@echo "live-accept is an unimplemented scaffold; it does not exercise profiles"
	go test -tags=live -count=1 -timeout=30m ./internal/acceptance/live

clean:
	rm -rf $(BUILD_DIR)
