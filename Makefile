# Matchlock Makefile
# This Makefile delegates to mise for task execution.
# Install mise: https://mise.jdx.dev/getting-started.html
# Then run: mise install

.PHONY: help
help:
	@echo "Matchlock uses mise for task management."
	@echo ""
	@echo "Setup:"
	@echo "  1. Install mise: https://mise.jdx.dev/getting-started.html"
	@echo "  2. Run: mise install"
	@echo "  3. Run: mise tasks"
	@echo ""
	@echo "Common tasks:"
	@echo "  mise run build           Build the matchlock CLI"
	@echo "  mise run test            Run all tests"
	@echo "  mise run lint            Run golangci-lint"
	@echo "  mise run kernel:build    Build kernels for all architectures"
	@echo "  mise run kernel:publish  Publish kernels to GHCR"
	@echo ""
	@echo "For backwards compatibility, make targets delegate to mise:"
	@mise tasks 2>/dev/null || echo "mise not installed - see https://mise.jdx.dev"

# Delegate all targets to mise
.PHONY: build build-all clean test lint fmt vet tidy check
.PHONY: kernel kernel-x86_64 kernel-arm64 kernel-publish kernel-clean
.PHONY: install setup images

build:
	@mise run build

build-all:
	@mise run build:all

clean:
	@mise run clean

test:
	@mise run test

lint:
	@mise run lint

fmt:
	@mise run fmt

vet:
	@mise run vet

tidy:
	@mise run tidy

check:
	@mise run check

kernel:
	@mise run kernel:build

kernel-x86_64:
	@mise run kernel:x86_64

kernel-arm64:
	@mise run kernel:arm64

kernel-publish:
	@mise run kernel:publish

kernel-clean:
	@mise run kernel:clean

install:
	@mise run install

setup:
	@mise run setup

images:
	@mise run images
