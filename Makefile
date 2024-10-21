include ./.bingo/Variables.mk
SHELL = /bin/bash

# This build tag is currently leveraged by tooling/image-sync
# https://github.com/containers/image?tab=readme-ov-file#building
GOTAGS?='containers_image_openpgp'
TOOLS_BIN_DIR := tooling/bin

all: test lint

# There is currently no convenient way to run tests against a whole Go workspace
# https://github.com/golang/go/issues/50745
test:
	go list -f '{{.Dir}}/...' -m | xargs go test -tags=$(GOTAGS) -covertig

# There is currently no convenient way to run golangci-lint against a whole Go workspace
# https://github.com/golang/go/issues/50745
MODULES := $(shell go list -f '{{.Dir}}/...' -m | xargs)
lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run -v --build-tags=$(GOTAGS) $(MODULES)

fmt: $(GOIMPORTS)
	$(GOIMPORTS) -w -local github.com/Azure/ARO-HCP $(shell go list -f '{{.Dir}}' -m | xargs)

.PHONY: all clean lint test fmt

_output:
	mkdir -p _output

_output/RegionAgnosticServiceModel.json _output/RegionAgnosticRolloutSpecification.json:
	wget --output-document=$@ --quiet https://ev2schema.azure.net/schemas/2020-04-01/$(notdir $@)

_output/service_model.go: _output/RegionAgnosticServiceModel.json $(GO_JSONSCHEMA)
	$(GO_JSONSCHEMA) _output/RegionAgnosticServiceModel.json --output _output/service_model.go --package github.com/ARO-HCP/_output

_output/onFailure: _output/RegionAgnosticRolloutSpecification.json
	jq --raw-output '.properties.onFailure' <_output/RegionAgnosticRolloutSpecification.json >_output/onFailure

_output/RegionAgnosticRolloutSpecification.modified.json: _output/onFailure
	jq --slurpfile onFailure _output/onFailure '.["$$defs"]["onFailure"]=$$onFailure[0]' <_output/RegionAgnosticRolloutSpecification.json >_output/RegionAgnosticRolloutSpecification.modified.json
	sed -i 's|#/properties/onFailure|#/$$defs/onFailure|g' _output/RegionAgnosticRolloutSpecification.modified.json

_output/rollout_spec.go: _output/RegionAgnosticRolloutSpecification.modified.json $(GO_JSONSCHEMA)
	$(GO_JSONSCHEMA) _output/RegionAgnosticRolloutSpecification.modified.json --output _output/rollout_spec.go --package github.com/ARO-HCP/_output