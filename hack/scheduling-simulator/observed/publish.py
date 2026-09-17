"""Publish already-collected regional windows without any network requests."""
import json
import time


def publish_regions(root, output, allow_partial=False):
    from .cli import iso, publish, write_json

    selection = json.loads((root / "peak-search.json").read_text())["selection"]
    if selection.get("scope") != "regional":
        raise ValueError("Expected a regional peak search")
    merged = {"schema_version": 1, "mode": "observed", "at": None, "start": None,
              "window_seconds": selection["window_seconds"], "step_seconds": selection["step_seconds"],
              "generated_at": iso(time.time()), "sources": [], "errors": [],
              "warnings": ["Regional windows are from different times, not simultaneous fleet utilization."],
              "transitions": [], "suggestion": None, "regional_windows": [], "management_clusters": []}
    for regional in selection["regions"]:
        env, region = regional["environment"], regional["region"]
        directory = root / "regions" / f"{env}-{region}"
        original = regional["original_selected_at"]
        candidates = sorted((p for p in directory.glob("*") if p.is_dir() and p.name.isdigit()
                             and (p / "raw.json").exists()), key=lambda p: (abs(int(p.name) - original), int(p.name)))
        entry = {"environment": env, "region": region, "status": "blocked", "peak_selection": dict(regional)}
        merged["regional_windows"].append(entry)
        chosen = None
        for path in candidates:
            raw = json.loads((path / "raw.json").read_text())
            if any("did not finish" in e for e in raw.get("errors", [])):
                continue
            if raw.get("window_seconds") != selection["window_seconds"]:
                continue
            chosen = path
            break
        if chosen is None:
            entry["reason"] = "No completed regional capture available"
            merged["errors"].append(f"{env}/{region}: {entry['reason']}")
            continue
        # Keep processing caches next to the source, not in the served bundle.
        destination = directory / "published" / chosen.name
        destination.mkdir(parents=True, exist_ok=True)
        print(f"Processing cached {env}/{region}: {iso(raw['start'])} .. {iso(raw['at'])}", flush=True)
        publish(raw, destination, allow_partial)
        entry.update(at=iso(raw["at"]), start=iso(raw["start"]), raw=str(chosen / "raw.json"))
        entry["peak_selection"].update(adjusted_at=raw["at"], adjusted_start=raw["start"],
                                       adjustment_seconds=raw["at"] - original)
        if raw["at"] != original:
            merged["warnings"].append(f"{env}/{region}: showing the available shifted capture, not the original ranked peak")
        manifest = json.loads((destination / "manifest.json").read_text())
        merged["errors"].extend(f"{env}/{region}: {e}" for e in manifest["errors"])
        if not (destination / "view.json").exists():
            entry["reason"] = "Coverage checks prevented publication"
            continue
        view = json.loads((destination / "view.json").read_text())
        entry["status"] = "partial" if view["errors"] else "success"
        for mc in view["management_clusters"]:
            mc.update(at=entry["at"], start=entry["start"], peak_selection=entry["peak_selection"])
            merged["management_clusters"].append(mc)
        merged["transitions"].extend(view["transitions"])
        merged["warnings"].extend(view["warnings"])
        for source in view["sources"]:
            if source not in merged["sources"]:
                merged["sources"].append(source)
    output.mkdir(parents=True, exist_ok=True)
    write_json(output / "regional-manifest.json", {k: v for k, v in merged.items() if k != "management_clusters"})
    if not merged["management_clusters"] or (merged["errors"] and not allow_partial):
        raise ValueError("No permitted regional view; inspect regional-manifest.json or use --allow-partial")
    write_json(output / "view.json", merged)
    print(f"Published {len(merged['management_clusters'])} regional MCs to {output / 'view.json'}", flush=True)
    return 2 if merged["errors"] else 0
