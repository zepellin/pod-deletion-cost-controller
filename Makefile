BIN_DIR            := bin
ENVTEST_K8S_VERSION := 1.33.x
ENVTEST_BIN        := $(BIN_DIR)/setup-envtest
ENVTEST_ASSETS     := $(BIN_DIR)/envtest-assets

.PHONY: all build test test-unit test-integration clean

all: build

build:
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/controller ./cmd/controller

# Unit tests only — no envtest binaries required.
test-unit:
	go test ./internal/config/... ./internal/metrics/...

# Full test suite including envtest integration tests.
test: test-unit $(ENVTEST_BIN)
	KUBEBUILDER_ASSETS="$$($(ENVTEST_BIN) use $(ENVTEST_K8S_VERSION) \
		--bin-dir $(abspath $(ENVTEST_ASSETS)) -p path)" \
		go test ./...

# Download the setup-envtest tool if not present.
$(ENVTEST_BIN):
	@mkdir -p $(BIN_DIR) $(ENVTEST_ASSETS)
	GOBIN=$(abspath $(BIN_DIR)) go install \
		sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

clean:
	rm -rf $(BIN_DIR)
