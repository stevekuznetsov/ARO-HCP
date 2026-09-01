.PHONY: build collect collect-int collect-prod collect-all process render report all clean help

BINARY_NAME := cluster-utilization
CACHE_DIR ?= .cache
WINDOW ?= 14d
STEP ?= 5m
DATASOURCE_PATTERN ?= ^(services|hcps)-

# Known ARO-HCP Grafana instances. Each environment is a separate Azure Managed
# Grafana instance; datasource UIDs collide across them, so the cache namespaces
# by instance automatically.
GRAFANA_INT  := https://arohcp-int-gyexf6e6ayh0hja3.suk.grafana.azure.com
GRAFANA_PROD := https://arohcp-prod-g5d9a9akashnb5gd.suk.grafana.azure.com

# GRAFANA_URL is used by the generic `collect` target.
GRAFANA_URL ?= $(GRAFANA_INT)

# Auto-default Azure credential selection for local `az login` if unset.
export AZURE_TOKEN_CREDENTIALS ?= dev

COLLECT_FLAGS := --cache-dir $(CACHE_DIR) --window $(WINDOW) --step $(STEP) --datasource-pattern '$(DATASOURCE_PATTERN)'

.DEFAULT_GOAL := help

build:
	@go build -o $(BINARY_NAME) .

collect: build
	@./$(BINARY_NAME) collect --grafana-url "$(GRAFANA_URL)" $(COLLECT_FLAGS)

collect-int: build
	@./$(BINARY_NAME) collect --grafana-url "$(GRAFANA_INT)" $(COLLECT_FLAGS)

collect-prod: build
	@./$(BINARY_NAME) collect --grafana-url "$(GRAFANA_PROD)" $(COLLECT_FLAGS)

## collect-all: pull int + prod into one cache dir (env/region derived from data)
collect-all: collect-int collect-prod

process: build
	@./$(BINARY_NAME) process --cache-dir $(CACHE_DIR)

render: build
	@./$(BINARY_NAME) render

## report: process the cache and render the HTML (offline; no Grafana needed)
report: process render

## all: full reproducible pipeline -> report.html covering int + prod
all: collect-all report

clean:
	@rm -f $(BINARY_NAME)

help:
	@echo "Reproducible pipeline:"
	@echo "  make all           - collect int+prod, process, render report.html"
	@echo ""
	@echo "Stage by stage:"
	@echo "  make collect-int   - pull the int Grafana into $(CACHE_DIR)/<instance>/"
	@echo "  make collect-prod  - pull the prod Grafana into $(CACHE_DIR)/<instance>/"
	@echo "  make collect-all   - both of the above"
	@echo "  make report        - process the cache + render report.html (offline)"
	@echo ""
	@echo "Variables: CACHE_DIR, WINDOW, STEP, DATASOURCE_PATTERN, GRAFANA_URL"
	@echo "Add --refresh via: ./$(BINARY_NAME) collect --grafana-url ... --refresh"
