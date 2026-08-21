.PHONY: all build build-login build-all clean install install-login help

PLUGIN_NAME = claude-sub
LOGIN_NAME = claude-sub-login
OUTPUT_DIR = build

# Platform detection
UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Linux)
	PLUGIN_EXT = .so
	PLATFORM = linux
endif
ifeq ($(UNAME_S),Darwin)
	PLUGIN_EXT = .so
	PLATFORM = darwin
endif

# Architecture detection
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_M),x86_64)
	ARCH = amd64
endif
ifeq ($(UNAME_M),arm64)
	ARCH = arm64
endif

OUTPUT = $(OUTPUT_DIR)/$(PLUGIN_NAME)$(PLUGIN_EXT)
LOGIN_OUTPUT = $(OUTPUT_DIR)/$(LOGIN_NAME)

all: build-login

build: ## Build the plugin for current platform
	@echo "Building plugin for $(PLATFORM)/$(ARCH)..."
	@mkdir -p $(OUTPUT_DIR)
	go build -buildmode=plugin -o $(OUTPUT) .
	@echo "Plugin built successfully: $(OUTPUT)"

build-login: ## Build the login CLI tool
	@echo "Building login CLI for $(PLATFORM)/$(ARCH)..."
	@mkdir -p $(OUTPUT_DIR)
	go build -o $(LOGIN_OUTPUT) ./cmd/claude-sub-login/
	@echo "Login CLI built successfully: $(LOGIN_OUTPUT)"

build-all: build build-login ## Build both plugin and login CLI

clean: ## Remove build artifacts
	@rm -rf $(OUTPUT_DIR)

install: build ## Build and install plugin to Bifrost plugins directory
	@mkdir -p ~/.bifrost/plugins
	@cp $(OUTPUT) ~/.bifrost/plugins/
	@echo "Plugin installed to ~/.bifrost/plugins/"

install-login: build-login ## Build and install login CLI to ~/.bifrost/bin/
	@mkdir -p ~/.bifrost/bin
	@cp $(LOGIN_OUTPUT) ~/.bifrost/bin/
	@echo "Login CLI installed to ~/.bifrost/bin/"

install-all: install install-login ## Install both plugin and login CLI
