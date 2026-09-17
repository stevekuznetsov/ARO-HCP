"""Allocate the packed MC worker-VM bill, not a marginal or price-optimal cost."""
import json
import math
from pathlib import Path


PRICES = json.loads((Path(__file__).parent / "sample_inputs/prices.json").read_text())
MONTH_HOURS = 730


def cost_summary(result, cfg, model, prices=PRICES):
    dims = ("cpu_mc", "mem_mib", "nic", "pods")
    rows = {}
    for size in sorted(model.sizes, key=int):
        demand = model.cluster_demand(size, result["policy"], cfg.percentile,
                                      cfg.multiplier, unsteered_placement=cfg.unsteered_placement)
        z, o = demand.zonal(), demand.overflow()
        rows[size] = {"size": size, "count": 0, "cpu": (z["cpu_mc"] + o["cpu_mc"]) / 1000,
                      "mem": (z["mem_mib"] + o["mem_mib"]) / 1024,
                      "zonal_cpu": z["cpu_mc"] / 1000, "zonal_mem": z["mem_mib"] / 1024,
                      "pods": z["pods"] + o["pods"], "nic": z["nic"] + o["nic"],
                      "zonal_hourly": 0.0, "overflow_hourly": 0.0}
    total = 0.0
    missing = set()
    unavailable = False
    inventory = {}
    for mc in result["management_clusters"]:
        for size, count in mc["hcp_mix"].items():
            rows[size]["count"] += count * mc["count"]
        if not mc.get("packing") or mc["zonal_pool"] is None or mc["overflow_pool"] is None:
            unavailable = True
            continue
        pk = mc["packing"]
        for role, nodes in (("zonal", [n for az in pk["zonal"] for n in az]),
                            ("overflow", pk["overflow"])):
            bill = 0.0
            for node in nodes:
                sku = node["sku"]
                inventory[sku] = inventory.get(sku, 0) + mc["count"]
                rate = prices["hourly"].get(sku)
                if rate is None or not math.isfinite(rate) or rate < 0:
                    missing.add(sku)
                else:
                    bill += rate
            total += bill * mc["count"]
            caps = {d: sum(n["cap"][d] for n in nodes) for d in dims}
            demand = {size: dict.fromkeys(dims, 0.0) for size in mc["hcp_mix"]}
            for node in nodes:
                for pod in node["pods"]:
                    if pod["reserve"]:
                        continue
                    for d in dims:
                        demand[pod["hcp_size"]][d] += 1 if d == "pods" else pod[d]
            # Normalize each size's dominant share of this pool's usable capacity.
            # All non-working capacity is amortized too, including placeholder HCPs.
            weights = {s: max((v[d] / caps[d] if caps[d] > 0 else 0) for d in dims)
                       for s, v in demand.items()}
            if not sum(weights.values()):
                weights = mc["hcp_mix"]  # e.g. a pool containing only reserved pods
            total_weight = sum(weights.values())
            for size, weight in weights.items():
                rows[size][role + "_hourly"] += bill * weight / total_weight * mc["count"]
    error = ("No estimate: missing or invalid hourly prices for " + ", ".join(sorted(missing))) if missing else None
    if unavailable:
        error = "No estimate: one or more MC pools could not be packed. Adjust capacity inputs."
    for row in rows.values():
        if error or not row["count"]:
            row["hourly"] = row["monthly"] = None
            row["zonal_hourly"] = row["overflow_hourly"] = None
        else:
            row["zonal_hourly"] /= row["count"]
            row["overflow_hourly"] /= row["count"]
            row["hourly"] = row["zonal_hourly"] + row["overflow_hourly"]
            row["monthly"] = row["hourly"] * MONTH_HOURS
    return {"rows": list(rows.values()), "error": error, "inventory": inventory,
            "hourly": None if error else total, "monthly": None if error else total * MONTH_HOURS}
