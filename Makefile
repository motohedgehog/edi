PACKAGES := ./cmd/... ./internal/... ./migrations/...
CONTAINER_ENGINE ?= $(if $(filter Darwin,$(shell uname -s)),container,docker)

.PHONY: fmt fmt-check test test-race build run container check

fmt:
	golangci-lint fmt $(PACKAGES)

fmt-check:
	golangci-lint fmt --diff $(PACKAGES)

test:
	go test -shuffle=on $(PACKAGES)

test-race:
	go test -race -shuffle=on $(PACKAGES)

check:
	golangci-lint config verify
	golangci-lint run $(PACKAGES)

build:
	go build -o bin/edi ./cmd/edi

run:
	@$(if $(filter 1 true,$(USE_OP)),op run --env-file .env --) go run ./cmd/edi

container:
	$(CONTAINER_ENGINE) build --tag edi:local .
