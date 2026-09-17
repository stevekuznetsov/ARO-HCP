#!/usr/bin/env python3
"""Plot kube-apiserver replica CPU & memory across scale tests, auto-detect the
steady-state window from the derivative of a smoothed trend, highlight it, and
overlay a boxplot of the steady-state distribution at the window midpoint."""
import json, warnings
import numpy as np
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.patches import Patch
from datasets import ORDER, KAS_CACHE, FIG_STEADY

DT = 30.0  # resample grid, seconds

def moving_average(y, w):
    n = len(y)
    if w < 2:
        return y.copy()
    half = w // 2
    out = np.full(n, np.nan)
    for i in range(n):
        lo, hi = max(0, i - half), min(n, i + half + 1)
        seg = y[lo:hi]
        seg = seg[~np.isnan(seg)]
        if seg.size:
            out[i] = seg.mean()
    return out

def build_signal(series):
    all_t = np.concatenate([np.array(v)[:, 0] for v in series.values()])
    t0, t1 = all_t.min(), all_t.max()
    grid = np.arange(0.0, t1 - t0 + DT, DT)
    reps = []
    for pod, pts in series.items():
        arr = np.array(pts)
        te = arr[:, 0] - t0
        ve = arr[:, 1]
        g = np.interp(grid, te, ve, left=np.nan, right=np.nan)
        g[(grid < te.min() - 1) | (grid > te.max() + 1)] = np.nan
        reps.append(g)
    reps = np.array(reps)
    with warnings.catch_warnings():
        warnings.simplefilter("ignore", category=RuntimeWarning)
        mean_sig = np.nanmean(reps, axis=0)
    return grid / 60.0, mean_sig, reps, t0

def detect_steady(t_min, mem_sig):
    """Detect the steady-state window from the MEMORY trend (a clean ramp->plateau).
    Smooth -> derivative -> find where the trend has reached its final plateau
    (>=95%) with a near-zero slope and stays there to the end."""
    good = ~np.isnan(mem_sig)
    tg, yg = t_min[good], mem_sig[good]
    if yg.size < 6:
        return tg[0], tg[-1]
    w = max(3, int(round(5 * 60 / DT)))                # ~5 min smoothing window
    trend = moving_average(yg, w)
    dydt = np.gradient(trend, tg)                       # slope per minute
    plateau = np.median(trend[int(0.8 * len(trend)):])
    reached = trend >= 0.95 * plateau
    slope_ok = np.abs(dydt) / max(plateau, 1e-9) < 0.015
    cond = reached & slope_ok
    n = len(tg)
    start_i = n - 1
    for i in range(n):
        if cond[i:].mean() > 0.85:
            start_i = i
            break
    return tg[start_i], tg[-1]

def main():
    data = json.load(open(KAS_CACHE))
    labels = [l for l in ORDER if l in data]
    nrows = len(labels)
    fig, axes = plt.subplots(nrows, 2, figsize=(15, 3.0 * nrows), squeeze=False)
    metrics = [("cpu", "CPU (millicores)", "tab:blue"),
               ("mem", "Memory working set (MiB)", "tab:red")]

    summary = {}
    for r, lab in enumerate(labels):
        # detect ONE steady-state window per test, from the memory signal
        t_mem, mem_mean, _, _ = build_signal(data[lab]["mem"])
        s0, s1 = detect_steady(t_mem, mem_mean)
        for c, (mkey, ylabel, color) in enumerate(metrics):
            ax = axes[r][c]
            t, mean_sig, reps, _ = build_signal(data[lab][mkey])
            for g in reps:
                ax.plot(t, g, lw=0.8, alpha=0.55, color=color)
            ax.plot(t, mean_sig, lw=1.8, color="black", alpha=0.7)
            mask = (t >= s0) & (t <= s1)
            ax.axvspan(s0, s1, color="green", alpha=0.12, zorder=0)
            ax.axvline(s0, color="green", ls="--", lw=0.8, alpha=0.6)
            ax.axvline(s1, color="green", ls="--", lw=0.8, alpha=0.6)
            pooled = reps[:, mask].ravel()
            pooled = pooled[~np.isnan(pooled)]
            mid = 0.5 * (s0 + s1)
            extent = max(s1 - s0, 1.0)
            if pooled.size:
                bp = ax.boxplot(pooled, positions=[mid], widths=extent * 0.16,
                                orientation="vertical", patch_artist=True,
                                manage_ticks=False, showfliers=False, zorder=6)
                for b in bp["boxes"]:
                    b.set(facecolor="white", alpha=0.55, edgecolor="black", lw=1.2)
                for med in bp["medians"]:
                    med.set(color="darkgreen", lw=2.2)
                for wh in bp["whiskers"]:
                    wh.set(color="black", lw=1.0)
                for cap in bp["caps"]:
                    cap.set(color="black", lw=1.0)
                p50 = np.median(pooled)
                ax.annotate(f"p50={p50:,.0f}", xy=(mid + extent * 0.09, p50),
                            xytext=(3, 0), textcoords="offset points",
                            fontsize=8, color="darkgreen", weight="bold",
                            va="center", ha="left",
                            bbox=dict(boxstyle="round,pad=0.15", fc="white", ec="darkgreen", alpha=0.85))
                summary.setdefault(lab, {})[mkey] = (s0, s1, p50, pooled.size)
            ax.set_ylabel(ylabel, fontsize=8)
            ax.set_ylim(bottom=0)
            ax.grid(True, alpha=0.25)
            ax.tick_params(labelsize=8)
            if r == 0:
                ax.set_title(["kube-apiserver CPU", "kube-apiserver Memory"][c], fontsize=11)
            if r == nrows - 1:
                ax.set_xlabel("elapsed time (minutes)", fontsize=9)
            ax.text(0.01, 0.97, f"{lab} nodes\nsteady {s0:.0f}\u2013{s1:.0f} min",
                    transform=ax.transAxes, va="top", ha="left", fontsize=9, weight="bold",
                    bbox=dict(boxstyle="round", fc="white", ec="0.7", alpha=0.85))

    handles = [plt.Line2D([], [], color="black", lw=1.8, alpha=0.7, label="mean of replicas"),
               plt.Line2D([], [], color="tab:blue", lw=0.8, alpha=0.6, label="individual replica"),
               Patch(facecolor="green", alpha=0.15, label="detected steady-state"),
               Patch(facecolor="white", edgecolor="black", label="steady-state boxplot")]
    fig.legend(handles=handles, loc="upper center", ncol=4, fontsize=9, bbox_to_anchor=(0.5, 1.0))
    fig.suptitle("ARO-HCP kube-apiserver replica usage vs. scale \u2014 steady-state detection",
                 fontsize=13, y=1.005)
    fig.tight_layout(rect=[0, 0, 1, 0.985])
    fig.savefig(FIG_STEADY, dpi=130, bbox_inches="tight")
    print("wrote", FIG_STEADY)
    print("\n=== steady-state summary (window minutes, p50) ===")
    for lab in labels:
        s = summary.get(lab, {})
        c = s.get("cpu"); m = s.get("mem")
        if c:
            print(f"{lab:>4} nodes  CPU  steady=[{c[0]:6.1f},{c[1]:6.1f}]min  p50={c[2]:9.1f} mc   n={c[3]}")
        if m:
            print(f"{lab:>4} nodes  MEM  steady=[{m[0]:6.1f},{m[1]:6.1f}]min  p50={m[2]:9.1f} MiB  n={m[3]}")

if __name__ == "__main__":
    main()
