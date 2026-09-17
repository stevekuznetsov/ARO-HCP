#!/usr/bin/env python3
"""kube-apiserver SUM across replicas (total) per scale test, with a boxplot of
the WHOLE-RUN distribution overlaid at the run midpoint. Separate output file."""
import json, warnings
import numpy as np
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.patches import Patch
from datasets import ORDER, KAS_CACHE, FIG_SUM

DT = 30.0  # resample grid, seconds

def build_sum(series):
    """Return (t_min, sum_signal); sum across replicas, valid only where all present."""
    all_t = np.concatenate([np.array(v)[:, 0] for v in series.values()])
    t0, t1 = all_t.min(), all_t.max()
    grid = np.arange(0.0, t1 - t0 + DT, DT)
    reps = []
    for pts in series.values():
        arr = np.array(pts)
        te, ve = arr[:, 0] - t0, arr[:, 1]
        g = np.interp(grid, te, ve, left=np.nan, right=np.nan)
        g[(grid < te.min() - 1) | (grid > te.max() + 1)] = np.nan
        reps.append(g)
    reps = np.array(reps)
    valid = np.sum(~np.isnan(reps), axis=0) == reps.shape[0]
    with warnings.catch_warnings():
        warnings.simplefilter("ignore", category=RuntimeWarning)
        ssum = np.nansum(reps, axis=0)
    ssum[~valid] = np.nan
    return grid / 60.0, ssum

def main():
    data = json.load(open(KAS_CACHE))
    labels = [l for l in ORDER if l in data]
    nrows = len(labels)
    fig, axes = plt.subplots(nrows, 2, figsize=(15, 3.0 * nrows), squeeze=False)
    metrics = [("cpu", "CPU (millicores)", "tab:blue"),
               ("mem", "Memory working set (MiB)", "tab:red")]
    summary = {}
    for r, lab in enumerate(labels):
        for c, (mkey, ylabel, color) in enumerate(metrics):
            ax = axes[r][c]
            t, ssum = build_sum(data[lab][mkey])
            good = ~np.isnan(ssum)
            ax.plot(t, ssum, lw=1.3, color=color, alpha=0.85)
            vals = ssum[good]; tg = t[good]
            if not tg.size:
                ax.set_ylabel(ylabel, fontsize=8); ax.set_ylim(bottom=0)
                ax.grid(True, alpha=0.25); ax.tick_params(labelsize=8)
                continue
            mid = 0.5 * (tg[0] + tg[-1])
            extent = tg[-1] - tg[0]
            if vals.size:
                bp = ax.boxplot(vals, positions=[mid], widths=extent * 0.16,
                                orientation="vertical", patch_artist=True,
                                manage_ticks=False, showfliers=False, zorder=6)
                for b in bp["boxes"]:
                    b.set(facecolor="white", alpha=0.55, edgecolor="black", lw=1.2)
                for m_ in bp["medians"]:
                    m_.set(color="darkgreen", lw=2.2)
                for w_ in bp["whiskers"]:
                    w_.set(color="black", lw=1.0)
                for cap in bp["caps"]:
                    cap.set(color="black", lw=1.0)
                p50 = np.median(vals)
                q1, q3 = np.percentile(vals, [25, 75])
                ax.annotate(f"p50={p50:,.0f}", xy=(mid + extent * 0.09, p50),
                            xytext=(3, 0), textcoords="offset points",
                            fontsize=8, color="darkgreen", weight="bold",
                            va="center", ha="left",
                            bbox=dict(boxstyle="round,pad=0.15", fc="white", ec="darkgreen", alpha=0.85))
                summary.setdefault(lab, {})[mkey] = (p50, q1, q3, vals.min(), vals.max())
            ax.set_ylabel(ylabel, fontsize=8)
            ax.set_ylim(bottom=0)
            ax.grid(True, alpha=0.25)
            ax.tick_params(labelsize=8)
            if r == 0:
                ax.set_title(["kube-apiserver CPU (sum of replicas)",
                              "kube-apiserver Memory (sum of replicas)"][c], fontsize=11)
            if r == nrows - 1:
                ax.set_xlabel("elapsed time (minutes)", fontsize=9)
            ax.text(0.01, 0.97, f"{lab} nodes", transform=ax.transAxes,
                    va="top", ha="left", fontsize=10, weight="bold",
                    bbox=dict(boxstyle="round", fc="white", ec="0.7", alpha=0.85))
    handles = [plt.Line2D([], [], color="tab:blue", lw=1.3, label="sum of replicas (total)"),
               Patch(facecolor="white", edgecolor="black", label="whole-run boxplot")]
    fig.legend(handles=handles, loc="upper center", ncol=2, fontsize=9, bbox_to_anchor=(0.5, 1.0))
    fig.suptitle("ARO-HCP kube-apiserver total (sum of replicas) usage vs. scale \u2014 whole-run distribution",
                 fontsize=13, y=1.005)
    fig.tight_layout(rect=[0, 0, 1, 0.985])
    fig.savefig(FIG_SUM, dpi=130, bbox_inches="tight")
    print("wrote", FIG_SUM)
    print("\n=== whole-run SUM-of-replicas distribution (p50 / IQR / min-max) ===")
    for lab in labels:
        s = summary.get(lab, {})
        for mk, unit in (("cpu", "mc"), ("mem", "MiB")):
            if mk in s:
                p50, q1, q3, lo, hi = s[mk]
                print(f"{lab:>4} nodes  {mk.upper():3}  p50={p50:10,.0f} {unit:4}  "
                      f"IQR=[{q1:,.0f}, {q3:,.0f}]  range=[{lo:,.0f}, {hi:,.0f}]")

if __name__ == "__main__":
    main()
