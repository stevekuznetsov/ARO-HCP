SHELL = /bin/bash
SHELLFLAGS = -eu -o pipefail

PROJECT_ROOT_DIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))

include $(PROJECT_ROOT_DIR)tooling/templatize/Makefile

CONFIG_FILE := $(PROJECT_ROOT_DIR)config/config.yaml
DEV_SETTINGS_FILE := $(PROJECT_ROOT_DIR)tooling/templatize/settings.yaml

DEPLOY_ENV ?= pers
ENV_MK_TMPL ?= Env.mk
LOG_LEVEL ?= "0"
# Resolve from this include, not the recursive make's working directory.
# Make input paths must not contain spaces.
ENV_CACHE_DIR ?= $(PROJECT_ROOT_DIR)_artifacts/env-cache
ifeq ($(filter /%,$(ENV_CACHE_DIR)),)
$(error ENV_CACHE_DIR must be an absolute path)
endif
HASH := $(shell printf '%s\0' "$(USER)" "$(DEPLOY_ENV)" "$(abspath $(ENV_MK_TMPL))" "$(LOG_LEVEL)" "$(abspath $(CONFIG_FILE))" "$(abspath $(DEV_SETTINGS_FILE))" | sha256sum | cut -d " " -f 1)
ENV_VARS_FILE ?= $(ENV_CACHE_DIR)/env.$(HASH).mk

# Target to generate the environment variables file
$(ENV_VARS_FILE): $(TEMPLATIZE) $(CONFIG_FILE) $(DEV_SETTINGS_FILE) $(ENV_MK_TMPL) $(filter-out $(ENV_VARS_FILE),$(MAKEFILE_LIST)) | private-env-cache
	@echo "generate env vars file $(ENV_VARS_FILE)"
	@echo "this might take a while the first time."
	@set -euo pipefail; umask 077; \
	output=$$(mktemp "$(ENV_VARS_FILE).XXXXXX"); \
	trap 'rm -f "$$output"' EXIT; \
	USER="$(USER)" $(TEMPLATIZE) generate \
		--config-file $(CONFIG_FILE) \
		--dev-settings-file $(DEV_SETTINGS_FILE) \
		--dev-environment $(DEPLOY_ENV) \
		--input $(ENV_MK_TMPL) \
		--output "$$output" \
		-v $(LOG_LEVEL); \
	mv -fT "$$output" "$(ENV_VARS_FILE)"

private-env-cache:
	@set -eu; umask 077; \
	dir="$(abspath $(dir $(ENV_VARS_FILE)))"; mkdir -p "$$dir"; \
	if [[ -L "$$dir" || ! -O "$$dir" || $$(stat -c %a "$$dir") != 700 ]]; then \
		echo "Environment cache directory must be owned by the current user, non-symlink and mode 0700: $$dir" >&2; exit 1; \
	fi; \
	[[ ! -L "$(ENV_VARS_FILE)" && ( ! -e "$(ENV_VARS_FILE)" || -f "$(ENV_VARS_FILE)" ) ]]
.PHONY: private-env-cache

# Do not continue with missing or stale variables after generation fails.
include $(ENV_VARS_FILE)
