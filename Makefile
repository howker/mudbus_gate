.PHONY: build build-all build-legacy test lint ci smoke simulate

APP=mbgw
BIN_DIR=bin
GOOS?=$(shell go env GOOS)
GOARCH?=$(shell go env GOARCH)

build:
	go build -mod=vendor -o $(BIN_DIR)/$(APP) ./cmd/mbgw

build-legacy:
	GOOS=windows GOARCH=amd64 go build -mod=vendor -o $(BIN_DIR)/$(APP)_win_legacy.exe ./cmd/mbgw

build-all: build build-legacy
	GOOS=linux GOARCH=amd64 go build -mod=vendor -o $(BIN_DIR)/$(APP)_linux_amd64 ./cmd/mbgw
	GOOS=linux GOARCH=arm64 go build -mod=vendor -o $(BIN_DIR)/$(APP)_linux_arm64 ./cmd/mbgw

test:
	go test -mod=vendor ./...

lint:
	go vet ./...

ci: build-all test lint

smoke:
	@echo "Running smoke tests..."
	./$(BIN_DIR)/$(APP) --help

simulate:
	go run -mod=vendor ./cmd/mbgw simulate $(D)
