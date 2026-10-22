SHELL = /bin/bash
SHELLFLAGS = -eu -o pipefail

ifndef zz_injected_EV2
ifndef RUNS_IN_TEMPLATIZE
SCRIPT_DIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
PROJECT_ROOT_DIR := $(realpath $(SCRIPT_DIR)/..)

DEPLOY_ENV ?= pers
PIPELINE ?= pipeline.yaml
PIPELINE_STEP ?= deploy
LOG_LEVEL ?= "0"
CONFIG_FILE ?= $(PROJECT_ROOT_DIR)/config/config.yaml
# templatize.sh always uses the checkout's settings and topology.
# Make input paths must not contain spaces.
ENV_CACHE_DIR ?= $(PROJECT_ROOT_DIR)/_artifacts/env-cache
ifeq ($(filter /%,$(ENV_CACHE_DIR)),)
$(error ENV_CACHE_DIR must be an absolute path)
endif
HASH := $(shell printf '%s\0' "$(USER)" "$(DEPLOY_ENV)" "$(abspath $(PIPELINE))" "$(PIPELINE_STEP)" "$(CURDIR)" "$(LOG_LEVEL)" "$(abspath $(CONFIG_FILE))" "$(PROJECT_ROOT_DIR)/tooling/templatize/settings.yaml" "$(PROJECT_ROOT_DIR)/topology.yaml" "$(REGION)" | sha256sum | cut -d " " -f 1)
ENV_VARS_FILE ?= $(ENV_CACHE_DIR)/deploy.$(HASH).cfg

# Target to generate the environment variables file
$(ENV_VARS_FILE): $(CONFIG_FILE) $(PROJECT_ROOT_DIR)/tooling/templatize/settings.yaml $(PROJECT_ROOT_DIR)/topology.yaml $(PIPELINE) $(PROJECT_ROOT_DIR)/templatize.sh $(filter-out $(ENV_VARS_FILE),$(MAKEFILE_LIST)) | private-env-cache
	@echo "generate env vars file ${ENV_VARS_FILE}"
	@echo "this might take a while the first time."
	@set -euo pipefail; umask 077; \
	output=$$(mktemp "$(ENV_VARS_FILE).XXXXXX"); \
	trap 'rm -f "$$output"' EXIT; \
	service_group=$$(yq .serviceGroup "$(PIPELINE)"); \
	USER="$(USER)" CONFIG_FILE="$(CONFIG_FILE)" LOG_LEVEL=${LOG_LEVEL} ${PROJECT_ROOT_DIR}/templatize.sh ${DEPLOY_ENV} \
		-p "$$service_group" \
		-s ${PIPELINE_STEP} \
		-o "$$output"; \
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
include ${ENV_VARS_FILE}
endif
endif
