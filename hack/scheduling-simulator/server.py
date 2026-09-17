#!/usr/bin/env python3
"""FastAPI server for the HCP scheduling simulator.

Run:
  ./venv/bin/uvicorn server:app --reload --port 8099
then open http://localhost:8099
"""
import os
import json
from dataclasses import asdict

from fastapi import FastAPI, Request, Form
from fastapi.responses import HTMLResponse
from fastapi.staticfiles import StaticFiles
from fastapi.templating import Jinja2Templates

from demand.model import DemandModel
from skus import load_catalog, catalog_to_dicts
from optimize.engine import RunConfig, compare_policies
from costing import PRICES, MONTH_HOURS, cost_summary

HERE = os.path.dirname(__file__)
app = FastAPI(title="HCP Scheduling Simulator")
app.mount("/static", StaticFiles(directory=os.path.join(HERE, "static")), name="static")
templates = Jinja2Templates(directory=os.path.join(HERE, "templates"))

MODEL = DemandModel()
SKUS = load_catalog()

DEFAULT_DISTRIBUTION = {"3": 0, "6": 0, "12": 200, "30": 150, "60": 80, "120": 40, "250": 10}

# Read once at process start. The served view never authenticates or runs queries.
OBSERVED_PATH = os.environ.get("OBSERVED_DATA", os.path.join(HERE, "observed-data", "view.json"))
OBSERVED_VIEW, OBSERVED_ERROR = None, None
try:
    with open(OBSERVED_PATH) as stream:
        OBSERVED_VIEW = json.load(stream)
    if OBSERVED_VIEW.get("schema_version") != 1 or OBSERVED_VIEW.get("mode") != "observed":
        raise ValueError("unsupported observed snapshot schema")
except FileNotFoundError:
    manifest_path = os.path.join(os.path.dirname(OBSERVED_PATH), "manifest.json")
    if os.path.exists(manifest_path):
        OBSERVED_ERROR = "Collection bundle exists but no view was published. Inspect manifest.json for coverage errors; use --allow-partial only if you accept them."
except (OSError, ValueError, AttributeError) as exc:
    OBSERVED_VIEW = None
    OBSERVED_ERROR = str(exc)


@app.get("/observed", response_class=HTMLResponse)
def observed(request: Request):
    return templates.TemplateResponse(request, "observed.html", {
        "view": OBSERVED_VIEW, "load_error": OBSERVED_ERROR,
    })


@app.get("/", response_class=HTMLResponse)
def index(request: Request):
    cfg = RunConfig()
    return templates.TemplateResponse(request, "index.html", {
        "sizes": MODEL.sizes,
        "distribution": DEFAULT_DISTRIBUTION,
        "cfg": asdict(cfg),
        "skus": catalog_to_dicts(SKUS),
    })


@app.post("/solve", response_class=HTMLResponse)
async def solve(request: Request):
    form = await request.form()
    distribution = {}
    for size in MODEL.sizes:
        try:
            distribution[size] = int(form.get(f"count_{size}", 0) or 0)
        except ValueError:
            distribution[size] = 0

    def f(name, default, cast=float):
        try:
            return cast(form.get(name, default))
        except (TypeError, ValueError):
            return default

    cfg = RunConfig(
        percentile=f("percentile", 75.0),
        multiplier=f("multiplier", 1.0),
        hcps_per_mc=f("hcps_per_mc", 100, int),
        reserve_slots=f("reserve_slots", 5, int),
        reserve_size=(form.get("reserve_size") or "30"),
        reservation_mode=("scaled" if form.get("reservation_mode") == "scaled" else "flat"),
        system_reserved_cpu_mc=f("system_reserved_cpu_mc", 3000.0),
        system_reserved_mem_mib=f("system_reserved_mem_mib", 7550.0),
        scaled_reference_vcpu=f("scaled_reference_vcpu", 32, int),
        az_failure_reserve=f("az_failure_reserve", 0.50),
        rollout_surge=f("rollout_surge", 0.15),
        concurrent_rolling_hcps=f("concurrent_rolling_hcps", 1, int),
        overflow_az_count=f("overflow_az_count", 1, int),
        max_pods_per_node=f("max_pods_per_node", 225, int),
        node_overhead_cpu_mc=f("node_overhead_cpu_mc", 468.0),
        node_overhead_mem_mib=f("node_overhead_mem_mib", 3592.0),
        node_overhead_pods=f("node_overhead_pods", 11, int),
        buffer_cpu=f("buffer_cpu", 0.0),
        buffer_mem=f("buffer_mem", 0.10),
        buffer_nic=f("buffer_nic", 0.0),
        buffer_pods=f("buffer_pods", 0.0),
        unsteered_placement=("zonal" if form.get("unsteered_placement") == "zonal" else "overflow"),
        mode="exact",  # this view always uses the precise (FFD) packing
    )
    result = compare_policies(distribution, cfg, MODEL, SKUS)
    costs = {pol: cost_summary(result[pol], cfg, MODEL) for pol in ("minimal", "legacy")}
    return templates.TemplateResponse(request, "result.html", {
        "costs": costs, "prices": PRICES, "month_hours": MONTH_HOURS,
        "result": result,
        "cfg": asdict(cfg),
        "distribution": distribution,
    })


@app.get("/health")
def health():
    return {"ok": True, "sizes": MODEL.sizes, "skus": [s.name for s in SKUS]}


@app.get("/preview", response_class=HTMLResponse)
def preview(request: Request):
    cfg = RunConfig(mode="exact")
    result = compare_policies(DEFAULT_DISTRIBUTION, cfg, MODEL, SKUS)
    costs = {pol: cost_summary(result[pol], cfg, MODEL) for pol in ("minimal", "legacy")}
    return templates.TemplateResponse(request, "preview.html", {
        "costs": costs, "prices": PRICES, "month_hours": MONTH_HOURS,
        "result": result, "cfg": asdict(cfg), "distribution": DEFAULT_DISTRIBUTION,
    })
