# Matchlock Makefile

# Configuration
KERNEL_VERSION ?= 6.1.94
OUTPUT_DIR ?= /opt/sandbox
IMAGE ?= standard
GO ?= go

# Binary names
SANDBOX_BIN = bin/sandbox
GUEST_AGENT_BIN = bin/guest-agent
GUEST_FUSED_BIN = bin/guest-fused

# Default target
.PHONY: all
all: build

# =============================================================================
# Build targets
# =============================================================================

.PHONY: build
build: $(SANDBOX_BIN)

.PHONY: build-all
build-all: $(SANDBOX_BIN) $(GUEST_AGENT_BIN) $(GUEST_FUSED_BIN)

$(SANDBOX_BIN): $(shell find . -name '*.go' -not -path './cmd/guest-*')
	@mkdir -p bin
	$(GO) build -o $@ ./cmd/sandbox

$(GUEST_AGENT_BIN): cmd/guest-agent/main.go
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -o $@ ./cmd/guest-agent

$(GUEST_FUSED_BIN): cmd/guest-fused/main.go
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -o $@ ./cmd/guest-fused

.PHONY: clean
clean:
	rm -rf bin/

# =============================================================================
# Test targets
# =============================================================================

.PHONY: test
test:
	$(GO) test ./...

.PHONY: test-verbose
test-verbose:
	$(GO) test -v ./...

.PHONY: test-coverage
test-coverage:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

# =============================================================================
# Development targets
# =============================================================================

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: lint
lint:
	@which golangci-lint > /dev/null || (echo "Install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest" && exit 1)
	golangci-lint run

.PHONY: tidy
tidy:
	$(GO) mod tidy

# =============================================================================
# Image build targets
# =============================================================================

.PHONY: kernel
kernel:
	@echo "Building kernel $(KERNEL_VERSION)..."
	@mkdir -p $(OUTPUT_DIR)
	KERNEL_VERSION=$(KERNEL_VERSION) OUTPUT_DIR=$(OUTPUT_DIR) ./scripts/build-kernel.sh

.PHONY: rootfs
rootfs: guest-binaries
	@echo "Building $(IMAGE) rootfs..."
	@mkdir -p $(OUTPUT_DIR)
	@cp $(GUEST_AGENT_BIN) /tmp/guest-agent
	@cp $(GUEST_FUSED_BIN) /tmp/guest-fused
	sudo IMAGE=$(IMAGE) OUTPUT_DIR=$(OUTPUT_DIR) ./scripts/build-rootfs.sh

.PHONY: rootfs-minimal
rootfs-minimal: guest-binaries
	@$(MAKE) rootfs IMAGE=minimal

.PHONY: rootfs-standard
rootfs-standard: guest-binaries
	@$(MAKE) rootfs IMAGE=standard

.PHONY: rootfs-full
rootfs-full: guest-binaries
	@$(MAKE) rootfs IMAGE=full

.PHONY: guest-binaries
guest-binaries: $(GUEST_AGENT_BIN) $(GUEST_FUSED_BIN)

.PHONY: images
images: kernel rootfs-standard
	@echo "Images built in $(OUTPUT_DIR)"
	@ls -la $(OUTPUT_DIR)

# =============================================================================
# Installation targets
# =============================================================================

.PHONY: install-firecracker
install-firecracker:
	@echo "Installing Firecracker..."
	@./scripts/install-firecracker.sh

.PHONY: install
install: $(SANDBOX_BIN)
	@echo "Installing sandbox to /usr/local/bin..."
	sudo cp $(SANDBOX_BIN) /usr/local/bin/sandbox
	@echo "Installed. Run 'sandbox --help' to get started."

.PHONY: install-images
install-images:
	@echo "Installing images to $(OUTPUT_DIR)..."
	@mkdir -p $(OUTPUT_DIR)
	@if [ -f bin/kernel ]; then cp bin/kernel $(OUTPUT_DIR)/; fi
	@if [ -f bin/rootfs-*.ext4 ]; then cp bin/rootfs-*.ext4 $(OUTPUT_DIR)/; fi

# =============================================================================
# Docker-based builds (no root required for rootfs)
# =============================================================================

.PHONY: docker-rootfs
docker-rootfs: guest-binaries
	@echo "Building rootfs using Docker..."
	@cp $(GUEST_AGENT_BIN) /tmp/guest-agent
	@cp $(GUEST_FUSED_BIN) /tmp/guest-fused
	docker run --rm --privileged \
		-v /tmp:/tmp \
		-v $(PWD)/scripts:/scripts:ro \
		-v $(OUTPUT_DIR):$(OUTPUT_DIR) \
		-e IMAGE=$(IMAGE) \
		-e OUTPUT_DIR=$(OUTPUT_DIR) \
		alpine:3.19 \
		sh -c "apk add --no-cache bash e2fsprogs && /scripts/build-rootfs.sh"

# =============================================================================
# Quick start
# =============================================================================

.PHONY: setup
setup: install-firecracker images install
	@echo ""
	@echo "============================================"
	@echo "Matchlock setup complete!"
	@echo "============================================"
	@echo ""
	@echo "Environment variables (add to ~/.bashrc):"
	@echo "  export SANDBOX_KERNEL=$(OUTPUT_DIR)/kernel"
	@echo "  export SANDBOX_ROOTFS=$(OUTPUT_DIR)/rootfs-standard.ext4"
	@echo ""
	@echo "Test with:"
	@echo "  sudo sandbox run echo 'Hello from sandbox'"
	@echo ""

.PHONY: quick-test
quick-test: build
	@echo "Running quick test..."
	@if [ -f $(OUTPUT_DIR)/kernel ] && [ -f $(OUTPUT_DIR)/rootfs-standard.ext4 ]; then \
		echo "Images found, testing sandbox..."; \
		sudo SANDBOX_KERNEL=$(OUTPUT_DIR)/kernel SANDBOX_ROOTFS=$(OUTPUT_DIR)/rootfs-standard.ext4 \
			./$(SANDBOX_BIN) run echo "Sandbox works!"; \
	else \
		echo "Images not found. Run 'make images' first."; \
		exit 1; \
	fi

# =============================================================================
# Help
# =============================================================================

.PHONY: help
help:
	@echo "Matchlock Build System"
	@echo ""
	@echo "Build targets:"
	@echo "  make build          Build the sandbox CLI"
	@echo "  make build-all      Build CLI and guest binaries"
	@echo "  make clean          Remove built binaries"
	@echo ""
	@echo "Test targets:"
	@echo "  make test           Run all tests"
	@echo "  make test-verbose   Run tests with verbose output"
	@echo "  make test-coverage  Generate coverage report"
	@echo ""
	@echo "Development targets:"
	@echo "  make fmt            Format code"
	@echo "  make vet            Run go vet"
	@echo "  make lint           Run golangci-lint"
	@echo "  make tidy           Run go mod tidy"
	@echo ""
	@echo "Image build targets:"
	@echo "  make kernel         Build Linux kernel for Firecracker"
	@echo "  make rootfs         Build rootfs (requires sudo)"
	@echo "  make rootfs-minimal Build minimal rootfs"
	@echo "  make rootfs-standard Build standard rootfs (default)"
	@echo "  make rootfs-full    Build full rootfs with dev tools"
	@echo "  make images         Build kernel + standard rootfs"
	@echo "  make docker-rootfs  Build rootfs using Docker (no sudo)"
	@echo ""
	@echo "Installation targets:"
	@echo "  make install-firecracker  Install Firecracker binary"
	@echo "  make install              Install sandbox to /usr/local/bin"
	@echo "  make setup                Full setup (firecracker + images + install)"
	@echo ""
	@echo "Configuration:"
	@echo "  KERNEL_VERSION=$(KERNEL_VERSION)"
	@echo "  OUTPUT_DIR=$(OUTPUT_DIR)"
	@echo "  IMAGE=$(IMAGE)"
	@echo ""
	@echo "Examples:"
	@echo "  make images OUTPUT_DIR=./local-images"
	@echo "  make rootfs IMAGE=full"
	@echo "  make kernel KERNEL_VERSION=6.6.30"
