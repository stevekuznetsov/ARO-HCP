IMAGE ?= hcp-scheduling-simulator:latest
PORT ?= 8099
DL ?= $(HOME)/Downloads

.PHONY: help build run stop logs profiles dev venv clean

help:
	@echo "targets:"
	@echo "  make build      - build the container image"
	@echo "  make run        - run the container on http://localhost:$(PORT)"
	@echo "  make stop       - stop & remove the container"
	@echo "  make logs       - follow container logs"
	@echo "  make dev        - run locally with autoreload (needs 'make venv')"
	@echo "  make venv       - create a local python venv (python3.12)"
	@echo "  make profiles   - regenerate sample_inputs/profiles.json from the perf cache"

build:
	docker build -t $(IMAGE) .

run: build
	-docker rm -f hcpsim 2>/dev/null
	docker run -d --name hcpsim -p $(PORT):8099 $(IMAGE)
	@echo "open http://localhost:$(PORT)"

stop:
	-docker rm -f hcpsim

logs:
	docker logs -f hcpsim

# ---- local (non-container) development ----
venv:
	python3.12 -m venv venv
	./venv/bin/pip install -q --upgrade pip
	./venv/bin/pip install -q -r requirements.txt

dev:
	./venv/bin/uvicorn server:app --reload --port $(PORT)

# Regenerate demand profiles from the perf-analysis cache (hack/hcp-scale-analysis).
profiles:
	ARO_HCP_SCALE_POD_CACHE=$(DL)/controlplane-pod-timeseries.json ./venv/bin/python demand/build_profiles.py

# Print measured per-node non-HCP overhead from a cluster-utilization report.html.
overhead:
	./venv/bin/python demand/extract_node_overhead.py $(DL)/report.html mgmt

clean:
	rm -rf venv __pycache__ */__pycache__
