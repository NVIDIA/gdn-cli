BIN      := bin/gdn
MAIN     := ./cmd/gdn
CGO_ENABLED ?= 0
BUILD_TAGS ?= netgo osusergo
GOOS     ?= $(shell go env GOOS)
GOARCH   ?= $(shell go env GOARCH)
# Prefer the nearest semver tag (strip leading v to match GoReleaser's
# {{ .Version }}). Falls back to "dev" outside a git checkout. Override with
# make bin VERSION=...
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
LDFLAGS  := -trimpath -buildvcs=false -tags "$(BUILD_TAGS)" -ldflags "-s -w -buildid= -X github.com/nvidia/gdn-cli/src/cmd.version=$(VERSION)"
GOMODFLAGS := -mod=mod

.PHONY: help bin install bin-linux bin-all fmt lint test vendor clean

help::
	@echo "GDN CLI Make targets"
	@echo
	@echo "  make bin        Build $(BIN) for the current platform ($(GOOS)/$(GOARCH))"
	@echo "  make install    Install gdn into GOBIN/GOPATH/bin"
	@echo "  make bin-linux  Build Linux amd64 and arm64 binaries"
	@echo "  make bin-all    Build native bin/gdn plus linux, darwin, and windows binaries"
	@echo "  make fmt        Format Go files with gofmt"
	@echo "  make lint       Check gofmt and run go vet"
	@echo "  make test       Run Go tests"
	@echo "  make vendor     Populate local vendor/ dependencies"
	@echo "  make clean      Remove built binaries from bin/"
	@echo
	@echo "Examples:"
	@echo "  make bin"
	@echo "  make install"
	@echo "  make lint"
	@echo "  make vendor"
	@echo "  make bin VERSION=0.1.1"
	@echo "  make bin GOOS=linux GOARCH=arm64"
	@echo "  make bin-all"
	@echo "  gdn --help"

# Build for the current (or overridden) platform.
# Override at the command line: make bin GOOS=linux GOARCH=arm64
bin:
	@echo "Building $(BIN) ($(GOOS)/$(GOARCH), version $(VERSION))..."
	@CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GOMODFLAGS) $(LDFLAGS) -o $(BIN) $(MAIN)
	@echo "  -> $(BIN)"

install:
	@echo "Installing gdn (version $(VERSION))..."
	@GOBIN="$$(go env GOBIN)"; \
	if [ -z "$$GOBIN" ]; then GOPATH="$$(go env GOPATH)"; GOBIN="$${GOPATH%%:*}/bin"; fi; \
	mkdir -p "$$GOBIN"; \
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOMODFLAGS) $(LDFLAGS) -o "$$GOBIN/gdn" $(MAIN); \
	echo "  -> $$GOBIN/gdn"

# Build both Linux variants.
bin-linux:
	@echo "Building $(BIN)-linux-amd64 (linux/amd64, version $(VERSION))..."
	@CGO_ENABLED=$(CGO_ENABLED) GOOS=linux GOARCH=amd64 go build $(GOMODFLAGS) $(LDFLAGS) -o $(BIN)-linux-amd64 $(MAIN)
	@echo "  -> $(BIN)-linux-amd64"
	@echo "Building $(BIN)-linux-arm64 (linux/arm64, version $(VERSION))..."
	@CGO_ENABLED=$(CGO_ENABLED) GOOS=linux GOARCH=arm64 go build $(GOMODFLAGS) $(LDFLAGS) -o $(BIN)-linux-arm64 $(MAIN)
	@echo "  -> $(BIN)-linux-arm64"

# Cross-compile for all supported platforms, plus native bin/gdn.
bin-all: bin bin-linux
	@echo "Building $(BIN)-darwin-amd64 (darwin/amd64, version $(VERSION))..."
	@CGO_ENABLED=$(CGO_ENABLED) GOOS=darwin GOARCH=amd64 go build $(GOMODFLAGS) $(LDFLAGS) -o $(BIN)-darwin-amd64 $(MAIN)
	@echo "  -> $(BIN)-darwin-amd64"
	@echo "Building $(BIN)-darwin-arm64 (darwin/arm64, version $(VERSION))..."
	@CGO_ENABLED=$(CGO_ENABLED) GOOS=darwin GOARCH=arm64 go build $(GOMODFLAGS) $(LDFLAGS) -o $(BIN)-darwin-arm64 $(MAIN)
	@echo "  -> $(BIN)-darwin-arm64"
	@echo "Building $(BIN)-windows-amd64.exe (windows/amd64, version $(VERSION))..."
	@CGO_ENABLED=$(CGO_ENABLED) GOOS=windows GOARCH=amd64 go build $(GOMODFLAGS) $(LDFLAGS) -o $(BIN)-windows-amd64.exe $(MAIN)
	@echo "  -> $(BIN)-windows-amd64.exe"
	@echo "Building $(BIN)-windows-arm64.exe (windows/arm64, version $(VERSION))..."
	@CGO_ENABLED=$(CGO_ENABLED) GOOS=windows GOARCH=arm64 go build $(GOMODFLAGS) $(LDFLAGS) -o $(BIN)-windows-arm64.exe $(MAIN)
	@echo "  -> $(BIN)-windows-arm64.exe"

fmt:
	gofmt -w cmd src

lint:
	@test -z "$$(gofmt -l cmd src)" || { gofmt -l cmd src; exit 1; }
	go vet $(GOMODFLAGS) ./...

test:
	go test $(GOMODFLAGS) ./... -race -count 1 -parallel 4 -v

vendor:
	go mod vendor

clean:
	rm -f bin/*

# The e2e and perf harnesses are internal-only and are stripped from the public
# GitHub release via .gitattributes export-ignore. The leading `-` makes these
# includes optional so the exported Makefile still works without them, and the
# trailing position keeps `help` the default goal while letting each included
# file append its own `help::` lines after the ones above.
-include make/e2e.mk
-include make/perf.mk
