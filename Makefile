# altrdb-telegram-bot — developer and operator entry points.
#
# Everything here is a thin wrapper around the Go toolchain plus staticcheck, so
# a fresh checkout needs nothing but Go 1.25+ and network access to the module
# proxy. Run `make help` for the list.

APP        := altrdb-telegram-bot
BIN        := bin/$(APP)
MODULE     := altrdb-telegram-bot
GO         ?= go
GOFLAGS    ?=
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS    := -s -w \
              -X $(MODULE)/internal/config.Version=$(VERSION) \
              -X $(MODULE)/internal/config.Commit=$(COMMIT)

STATICCHECK     := $(GO) run honnef.co/go/tools/cmd/staticcheck@2025.1.1
DATABASE_PATH   ?= ./bot.db
ENV_FILE        ?= .env
IMAGE           ?= $(APP):$(VERSION)
COVERAGE_FILE   ?= coverage.out

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_.-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

##@ Build

.PHONY: build
build: ## Build the binary into bin/
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/altrdb-bot
	@echo "built $(BIN) ($(VERSION) $(COMMIT))"

.PHONY: install
install: ## Install the binary into GOBIN
	CGO_ENABLED=0 $(GO) install $(GOFLAGS) -trimpath -ldflags '$(LDFLAGS)' ./cmd/altrdb-bot

.PHONY: run
run: build ## Run locally with .env (create it from .env.example first)
	@set -a; [ -f $(ENV_FILE) ] && . ./$(ENV_FILE); set +a; \
		$(BIN)

.PHONY: clean
clean: ## Remove build and coverage artefacts
	rm -rf bin dist $(COVERAGE_FILE) coverage.html

##@ Quality

.PHONY: fmt
fmt: ## Format the tree with gofmt
	$(GO) fmt ./...
	@gofmt -l -w .

.PHONY: fmt-check
fmt-check: ## Fail when anything is unformatted
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: vet fmt-check ## Run go vet, gofmt -l and staticcheck
	$(STATICCHECK) ./...

.PHONY: test
test: ## Run the unit and integration tests
	$(GO) test $(GOFLAGS) ./...

.PHONY: test-race
test-race: ## Run the tests under the race detector
	$(GO) test $(GOFLAGS) -race -count=1 ./...

.PHONY: test-verbose
test-verbose: ## Run the tests verbosely
	$(GO) test $(GOFLAGS) -v -count=1 ./...

.PHONY: cover
cover: ## Write coverage.out and print the total
	$(GO) test $(GOFLAGS) -coverprofile=$(COVERAGE_FILE) -covermode=atomic ./...
	@$(GO) tool cover -func=$(COVERAGE_FILE) | tail -1

.PHONY: cover-html
cover-html: cover ## Write coverage.html
	$(GO) tool cover -html=$(COVERAGE_FILE) -o coverage.html
	@echo "wrote coverage.html"

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy
	@$(GO) mod verify

##@ Database

.PHONY: migrations
migrations: build ## Apply pending migrations to DATABASE_PATH and report the version
	DATABASE_PATH=$(DATABASE_PATH) $(BIN) -migrate

.PHONY: migrations-list
migrations-list: build ## List the embedded migrations
	$(BIN) -list-migrations

.PHONY: check-config
check-config: build ## Validate the current environment and exit
	@set -a; [ -f $(ENV_FILE) ] && . ./$(ENV_FILE); set +a; \
		$(BIN) -check-config

##@ Packaging

.PHONY: docker
docker: ## Build the OCI image (podman or docker)
	$(CONTAINER_TOOL) build $(CONTAINER_FORMAT_FLAG) \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE) .

.PHONY: docker-run
docker-run: ## Run the image with .env and a local volume
	$(CONTAINER_TOOL) run --rm -it \
		--env-file $(ENV_FILE) \
		-v altrdb-bot-data:/var/lib/altrdb-telegram-bot \
		-p 127.0.0.1:8080:8080 \
		$(IMAGE)

CONTAINER_TOOL ?= $(shell command -v docker 2>/dev/null || command -v podman 2>/dev/null)
# podman defaults to the OCI image format, which silently drops HEALTHCHECK.
# Ask for the docker format so the image carries its own probe.
ifeq ($(notdir $(CONTAINER_TOOL)),podman)
CONTAINER_FORMAT_FLAG := --format docker
else
CONTAINER_FORMAT_FLAG :=
endif
