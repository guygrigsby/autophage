.PHONY: help web-dev server-dev web-build server-build cli-build build \
        server-test web-test test check clean \
        install install-launchd uninstall-launchd redeploy service-restart dev

SHELL := /bin/bash

APP         ?= app
WEB_DIR     := web
INSTALL_DIR ?= $(HOME)/.local/bin
LAUNCHD_LABEL := dev.grigsby.$(APP)d
LAUNCHD_PLIST := $(HOME)/Library/LaunchAgents/$(LAUNCHD_LABEL).plist

# HAS_WEB is non-empty when this app embeds a web SPA. `init.sh --no-web` removes
# web/, so the build/check/test targets gate their web steps on this to work in
# both modes from one Makefile.
HAS_WEB := $(wildcard $(WEB_DIR)/package.json)
# Run golangci-lint v2 via `go run` so it is built with the module's own Go
# toolchain. A prebuilt binary built with an older Go refuses a newer go.mod
# ("the Go language version used to build golangci-lint is lower than the
# targeted Go version"). Override GOLANGCI to use a locally-installed binary.
# (Named GOLANGCI, not LINT: make predefines LINT=lint, which ?= won't override.)
GOLANGCI ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

$(WEB_DIR)/node_modules: $(WEB_DIR)/package.json
	cd $(WEB_DIR) && npm install
	@touch $@

web-dev: $(WEB_DIR)/node_modules ## Run the Vite dev server (proxies to :8080)
	cd $(WEB_DIR) && npm run dev

web-build: $(WEB_DIR)/node_modules ## Build the SPA into web/dist
	cd $(WEB_DIR) && npm run build
	@touch $(WEB_DIR)/dist/.gitkeep

server-build: ## Build the appd daemon (embeds web/dist)
	go build -o $(APP)d ./cmd/appd

cli-build: ## Build the app CLI
	go build -o $(APP) ./cmd/app

build: server-build cli-build ## Build both binaries (+ SPA when web/ is present)
	@if [ -n "$(HAS_WEB)" ]; then $(MAKE) web-build; fi

server-dev: ## Run appd from source
	go run ./cmd/appd

server-test: ## Run Go tests
	go test ./...

web-test: $(WEB_DIR)/node_modules ## Run the vitest suite
	cd $(WEB_DIR) && npm test

test: server-test ## Run all tests (Go, web when present, init smoke)
	@if [ -n "$(HAS_WEB)" ]; then $(MAKE) web-test; fi
	bash scripts/init_smoke_test.sh

check: ## One-shot quality gate for agents (run before claiming done)
	@test -z "$$(gofmt -l .)" || { echo "gofmt needs:"; gofmt -l .; exit 1; }
	go vet ./...
	$(GOLANGCI) run
	go test ./...
	@if [ -n "$(HAS_WEB)" ]; then $(MAKE) web-build; fi

install: build ## Install both binaries to INSTALL_DIR
	@mkdir -p $(INSTALL_DIR)
	cp $(APP)d $(APP) $(INSTALL_DIR)/
	@echo "✓ installed $(APP)d, $(APP) to $(INSTALL_DIR)"

install-launchd: install ## Install + load the appd LaunchAgent (macOS)
	@mkdir -p $(HOME)/Library/LaunchAgents $(HOME)/.logs/$(APP)
	@sed -e "s|{{INSTALL_DIR}}|$(INSTALL_DIR)|g" -e "s|{{HOME}}|$(HOME)|g" \
		deploy/$(LAUNCHD_LABEL).plist.template > $(LAUNCHD_PLIST)
	@echo "✓ wrote $(LAUNCHD_PLIST)"
	@echo "  Load:   launchctl load $(LAUNCHD_PLIST)"

uninstall-launchd: ## Unload + remove the appd LaunchAgent
	@if [ -f $(LAUNCHD_PLIST) ]; then \
		launchctl unload $(LAUNCHD_PLIST) 2>/dev/null || true; \
		rm $(LAUNCHD_PLIST); echo "✓ removed $(LAUNCHD_PLIST)"; \
	else echo "no plist at $(LAUNCHD_PLIST)"; fi

service-restart: ## Rebuild, reinstall, kickstart appd in place
	@$(MAKE) install
	@if launchctl list | awk '{print $$3}' | grep -qx "$(LAUNCHD_LABEL)"; then \
		launchctl kickstart -k gui/$$(id -u)/$(LAUNCHD_LABEL) && echo "✓ kickstarted $(LAUNCHD_LABEL)"; \
	else echo "$(LAUNCHD_LABEL) not loaded; run 'make install-launchd' first"; exit 1; fi

redeploy: ## Stop appd, install fresh binary, start it back up (clean swap)
	@if [ ! -f $(LAUNCHD_PLIST) ]; then echo "run 'make install-launchd' first"; exit 1; fi
	@launchctl bootout gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null || true
	@$(MAKE) install
	@i=0; while launchctl print gui/$$(id -u)/$(LAUNCHD_LABEL) >/dev/null 2>&1; do \
		i=$$((i+1)); if [ $$i -ge 50 ]; then echo "timed out"; exit 1; fi; sleep 0.1; done
	@launchctl bootstrap gui/$$(id -u) $(LAUNCHD_PLIST)
	@echo "✓ redeployed $(LAUNCHD_LABEL)"

dev: ## Run appd watcher + Vite together (both hot-reload)
	@trap 'kill 0' EXIT INT TERM; \
	  scripts/dev-watch.sh & \
	  $(MAKE) web-dev & \
	  wait

clean: ## Remove build artifacts
	rm -f $(APP)d $(APP)
	@find $(WEB_DIR)/dist -mindepth 1 ! -name .gitkeep -exec rm -rf {} + 2>/dev/null || true
	@echo "✓ clean"
