APP=mbgw
BIN_DIR=bin

# GO lets you pick the toolchain, e.g. `make build-legacy GO=go1.20.14`.
# Per FINAL_TRD.md the project targets Go 1.20.x so binaries run on
# Windows Server 2008 R2 / 2012 (Go 1.21+ requires Windows 10 / Server
# 2016 and crashes on boot on older Windows with 0xc0000005).
GO?=go

GOOS?=$(shell $(GO) env GOOS)
GOARCH?=$(shell $(GO) env GOARCH)

# Offline / vendor-first build environment (FINAL_TRD.md line 10):
# GOTOOLCHAIN=local pins the toolchain so a newer Go isn't silently
# fetched; GOPROXY=off forbids network module access; -mod=vendor uses
# the checked-in vendor/ tree.
GOENV=GOTOOLCHAIN=local GOFLAGS=-mod=vendor GOPROXY=off

build:
	$(GOENV) $(GO) build -mod=vendor -o $(BIN_DIR)/$(APP) ./cmd/mbgw

# Legacy Windows target. Build this with the Go 1.20 toolchain, e.g.:
#   make build-legacy GO=go1.20.14
build-legacy:
	$(GOENV) GOOS=windows GOARCH=amd64 $(GO) build -mod=vendor -o $(BIN_DIR)/$(APP)_win_legacy.exe ./cmd/mbgw

build-all: build build-legacy
	$(GOENV) GOOS=linux GOARCH=amd64 $(GO) build -mod=vendor -o $(BIN_DIR)/$(APP)_linux_amd64 ./cmd/mbgw
	$(GOENV) GOOS=linux GOARCH=arm64 $(GO) build -mod=vendor -o $(BIN_DIR)/$(APP)_linux_arm64 ./cmd/mbgw

# Regenerate the vendor/ tree (run after changing dependencies).
vendor:
	$(GO) mod tidy
	$(GO) mod vendor

test:
	$(GOENV) $(GO) test -mod=vendor ./...

lint:
	$(GOENV) $(GO) vet ./...

ci: build-all test lint

smoke:
	@echo "Running smoke tests..."
	./$(BIN_DIR)/$(APP) --help

simulate:
	$(GOENV) $(GO) run -mod=vendor ./cmd/mbgw simulate $(D)