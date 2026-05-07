TRIAGE_DIR := $(dir $(lastword $(MAKEFILE_LIST)))

TRIAGE_SOURCES = $(shell find $(TRIAGE_DIR) -name '*.go' -o -name 'go.mod' -o -name 'go.sum' -o -name '*.kql')

TRIAGE = $(TRIAGE_DIR)triage
$(TRIAGE): $(TRIAGE_SOURCES) $(MAKEFILE_LIST)
	cd $(TRIAGE_DIR) && go build -o triage main.go