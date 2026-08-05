BIN_DIR             := bin
ENVTEST_K8S_VERSION := 1.33.x
ENVTEST_ASSETS      := $(BIN_DIR)/envtest-assets

# setup-envtest is pinned by the tool directive in go.mod, so `go tool` builds
# the same version every run and Renovate can see it. Nothing to install.
ENVTEST := go tool setup-envtest

.PHONY: all build fmt lint test test-unit test-integration clean

all: build

build:
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/controller ./cmd/controller

fmt:
	gofmt -w .

# Formatting and static analysis. Fails if any file is not gofmt'd.
lint:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "Not gofmt'd:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...

# Unit tests only — no envtest binaries required.
test-unit:
	go test -race ./cmd/... ./internal/config/... ./internal/metrics/... \
		./internal/strategy/... ./internal/telemetry/...

# Full test suite including envtest integration tests.
test: test-unit
	@mkdir -p $(ENVTEST_ASSETS)
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) \
		--bin-dir $(abspath $(ENVTEST_ASSETS)) -p path)" \
		go test ./...

clean:
	rm -rf $(BIN_DIR)
