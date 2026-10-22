"use strict";

(() => {
  const data = JSON.parse(document.getElementById("dashboard-data").textContent);
  const byID = id => document.getElementById(id);
  const list = value => Array.isArray(value) ? value : [];
  const numeric = value => typeof value === "number" && Number.isFinite(value);
  const number = value => numeric(value) ? value.toLocaleString("en-US", { maximumFractionDigits: 3 }) : "Unknown";
  const duration = seconds => seconds >= 3600 ? `${number(seconds / 3600)} h` : `${number(seconds / 60)} min`;
  const unit = (value, metric) => {
    if (!numeric(value)) return "Unknown";
    if (metric === "cpu") return value > 0 && value < 1 ? `${number(value * 1000)} mCPU` : `${number(value)} cores`;
    if (["memory", "workingSet", "rss"].includes(metric)) return value >= 1024 ? `${number(value / 1024)} GiB` : `${number(value)} MiB`;
    return `${number(value)} ${metric || "s"}`;
  };
  const element = (tag, text, className) => {
    const node = document.createElement(tag);
    if (text !== undefined) node.textContent = text;
    if (className) node.className = className;
    return node;
  };
  const svgElement = (tag, attributes = {}) => {
    const node = document.createElementNS("http://www.w3.org/2000/svg", tag);
    for (const [key, value] of Object.entries(attributes)) node.setAttribute(key, value);
    return node;
  };
  const palette = ["#28665d", "#486e5e", "#3b6370", "#827343", "#916044", "#675c51", "#536b46", "#435b62"];
  const color = id => {
    let hash = 5381;
    for (const char of id) hash = ((hash * 33) ^ char.charCodeAt(0)) >>> 0;
    return palette[hash % palette.length];
  };
  let size;
  let selected;
  let selectedComponent;
  let scoped = [];
  let tab = "latency";
  const percentile = () => ["median", "q3", "p99"][Number(byID("percentile").value)];
  const percentileLabel = () => ["p50", "p75", "p99"][Number(byID("percentile").value)];
  const componentName = id => list(size.components).find(component => component.id === id)?.name || id;
  const runs = () => list(size.runs);
  const runLabel = run => `${number(run.offeredQPS)} QPS / ${run.population} / ${number(run.applicationPods)} Pods / ${number(run.applications)} apps / ${number(run.guestNodes)} nodes / ${duration(run.actualSeconds)} / case ${runs().indexOf(run) + 1} / ${run.kind}`;
  const selectOptions = (select, entries, value) => {
    select.replaceChildren(...entries.map(([key, label]) => {
      const option = element("option", label);
      option.value = key;
      return option;
    }));
    if (entries.some(([key]) => key === value)) select.value = value;
  };

  // Summaries use supplied quartiles and coverage. Never estimate quantiles or
  // silently convert missing observations/caps to zero in the browser.
  function statistics(distribution, metric, extra = {}) {
    const coverage = `censored ${number(extra.censored)}, excluded ${number(extra.excluded)}`;
    if (!distribution || distribution.count <= 0) return `Not tested/no completed samples; n ${number(distribution?.count)}; expected ${number(distribution?.expected)}; missing ${number(distribution?.missing)}; ${coverage}. ${extra.note || ""}`;
    return `n ${number(distribution.count)}; expected ${number(distribution.expected)}; missing ${number(distribution.missing)}; ` +
      `min ${unit(distribution.min, metric)}; p25 ${unit(distribution.q1, metric)}; p50 ${unit(distribution.median, metric)}; ` +
      `p75 ${unit(distribution.q3, metric)}; p95 ${unit(distribution.p95, metric)}; p99 ${unit(distribution.p99, metric)}; ` +
      `max ${unit(distribution.max, metric)}; whiskers ${unit(distribution.whiskerLow, metric)} to ${unit(distribution.whiskerHigh, metric)}; ` +
      `outliers ${number(distribution.outlierCount)}; ${coverage}. ${extra.note || ""}`;
  }

  function boxplot(distribution, maximum, metric, population, description, request, limit) {
    if (!distribution || distribution.count <= 0) return element("p", "Not tested/no completed samples", "empty");
    const svg = svgElement("svg", { viewBox: "0 0 340 32", preserveAspectRatio: "none", role: "img", tabindex: "0", "aria-label": description });
    svg.append(elementSVGTitle(description));
    const x = value => 12 + Math.max(0, Math.min(1, value / maximum)) * 316;
    const stroke = population === "unloaded" ? "#bd6133" : "#28796d";
    const line = (x1, y1, x2, y2, attrs = {}) => svg.append(svgElement("line", { x1, y1, x2, y2, stroke, ...attrs }));
    line(12, 31, 328, 31, { stroke: "#d7d9ce" });
    line(x(distribution.whiskerLow), 16, x(distribution.whiskerHigh), 16);
    for (const value of [distribution.whiskerLow, distribution.whiskerHigh]) line(x(value), 10, x(value), 22);
    svg.append(svgElement("rect", { x: x(distribution.q1), y: 7, width: Math.max(1, x(distribution.q3) - x(distribution.q1)), height: 18, fill: stroke, "fill-opacity": ".22", stroke }));
    line(x(distribution.median), 7, x(distribution.median), 25, { "stroke-width": "2" });
    svg.append(svgElement("circle", { cx: x(distribution.p95), cy: 16, r: 3, fill: "#a67a19", stroke: "#f7f5ee" }));
    if (numeric(request)) line(x(request), 3, x(request), 29, { stroke: "#203632", "stroke-dasharray": "3 2" });
    if (numeric(limit)) line(x(limit), 3, x(limit), 29, { stroke: "#203632", "stroke-width": "2" });
    const plot = element("div", undefined, "box-plot");
    const axis = element("div", undefined, "box-axis");
    axis.append(element("span", unit(0, metric)), element("span", unit(maximum, metric)));
    plot.append(svg, axis);
    return plot;
  }

  function elementSVGTitle(text) {
    const title = svgElement("title");
    title.textContent = text;
    return title;
  }

  function renderEnvelope() {
    const root = byID("envelope-chart");
    root.replaceChildren();
    if (!runs().length) {
      root.append(element("p", "No measured operating-envelope cases supplied.", "empty"));
      return;
    }
    const pods = [...new Set(runs().map(run => run.applicationPods))].sort((a, b) => b - a);
    const qps = [...new Set(runs().map(run => run.offeredQPS))].sort((a, b) => a - b);
    const groups = new Map();
    for (const run of runs()) {
      const key = `${run.applicationPods}/${run.offeredQPS}`;
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(run);
    }
    const laneHeight = Math.max(58, Math.max(...[...groups.values()].map(group => group.length)) * 22 + 16);
    const height = 50 + pods.length * laneHeight;
    const width = 760;
    const x = value => 105 + value / Math.max(1, ...qps) * (width - 140);
    const y = value => 24 + pods.indexOf(value) * laneHeight + laneHeight / 2;
    const svg = svgElement("svg", { viewBox: `0 0 ${width} ${height}`, role: "group", "aria-label": "Measured operating envelope. Individual run buttons at each offered QPS and application pod count." });
    const text = (value, px, py, anchor = "start") => {
      const node = svgElement("text", { x: px, y: py, "text-anchor": anchor });
      node.textContent = value;
      svg.append(node);
    };
    text("Application pods", 0, 13);
    for (const count of pods) {
      svg.append(svgElement("line", { x1: 95, x2: width - 22, y1: y(count), y2: y(count), class: "envelope-grid" }));
      text(number(count), 80, y(count) + 4, "end");
    }
    for (const value of [...new Set([0, ...qps])]) {
      svg.append(svgElement("line", { x1: x(value), x2: x(value), y1: 24, y2: height - 26, class: "envelope-grid" }));
      text(number(value), x(value), height - 11, "middle");
    }
    text("Offered QPS", width - 22, 13, "end");
    for (const group of groups.values()) {
      for (const [repeat, run] of group.entries()) {
        const interrupted = run.actualSeconds + 1 < run.plannedSeconds;
        const shape = interrupted ? "interrupted" : run.kind === "confirmation" ? "hour" : "screen";
        const hasThreshold = numeric(run.requests?.maxDegradedFraction);
        const status = hasThreshold && /^(pass|passed)$/i.test(run.assessment?.requestQuality) ? "met" :
          hasThreshold && /^(fail|failed)$/i.test(run.assessment?.requestQuality) ? "exceeded" : "unknown";
        const description = `${run.label}; ${runLabel(run)}; ${number(run.applicationPods)} application pods; repeat ${repeat + 1} of ${group.length} at this coordinate; ${run.cohort}; request quality: ${run.assessment?.requestQuality || "Unknown"}; health: ${run.assessment?.health || "Unknown"}; execution: ${run.assessment?.execution || "Unknown"}; capture: ${run.assessment?.capture || "Unknown"}.`;
        const point = svgElement("g", {
          transform: `translate(${x(run.offeredQPS)},${y(run.applicationPods) + (repeat - (group.length - 1) / 2) * 22})`,
          class: `envelope-point ${status}`, role: "button", tabindex: "0", "aria-label": description,
          "aria-pressed": String(run === selected), "data-run": run.id, "data-shape": shape,
          "data-qps": run.offeredQPS, "data-pods": run.applicationPods
        });
        point.append(elementSVGTitle(description));
        point.append(svgElement("circle", { r: 10, class: "point-halo" }));
        if (interrupted) point.append(svgElement("path", { d: "M-5,-5 L5,5 M-5,5 L5,-5", class: "point-cross" }));
        else point.append(svgElement("circle", { r: 5, class: shape === "hour" ? "point-ring" : "point-dot" }));
        point.addEventListener("click", () => chooseRun(run, true));
        point.addEventListener("keydown", event => {
          if (event.key === "Enter" || event.key === " ") {
            event.preventDefault();
            chooseRun(run, true);
          }
        });
        point.addEventListener("focus", () => { byID("envelope-selection").textContent = description; });
        point.addEventListener("mouseenter", () => { byID("envelope-selection").textContent = description; });
        point.addEventListener("mouseleave", () => { byID("envelope-selection").textContent = selected ? `Selected: ${selected.label} / ${runLabel(selected)} / health: ${selected.assessment?.health || "Unknown"}` : "Select a measured case."; });
        svg.append(point);
      }
    }
    root.append(svg);
  }

  function distributionPanel(title, cases, metric) {
    const panel = element("section", undefined, "chart-panel");
    panel.append(element("h3", title));
    const maximum = Math.max(0.001, ...cases.flatMap(item => [item.distribution?.whiskerHigh, item.distribution?.p95, item.request, item.limit].filter(numeric))) * 1.05;
    const details = element("details", undefined, "statistics");
    details.append(element("summary", "Statistics and sample coverage (all cases)"));
    if (!cases.length) panel.append(element("p", "No matching measured cases.", "empty"));
    for (const item of cases) {
      const row = element("div", undefined, "box-row");
      const label = element("div", undefined, "case-label");
      const button = element("button", runLabel(item.run));
      button.type = "button";
      button.title = item.run.label;
      button.addEventListener("click", () => chooseRun(item.run, false));
      label.append(button);
      let description = `${item.run.label}; ${runLabel(item.run)}; ${title}: ${statistics(item.distribution, metric, item.extra)}`;
      if (item.caps) description += ` Request ${unit(item.request, metric)}; limit ${unit(item.limit, metric)}.`;
      row.append(label, boxplot(item.distribution, maximum, metric, item.run.population, description, item.request, item.limit));
      panel.append(row);
      details.append(element("p", description));
    }
    panel.append(details);
    return panel;
  }

  function renderComparisons() {
    scoped = runs().filter(run => run.cohort === byID("cohort").value &&
      (byID("kind").value === "all" || run.kind === byID("kind").value) &&
      (byID("population").value === "compare" || run.population === byID("population").value) &&
      (byID("duration").value === "all" || String(run.plannedSeconds) === byID("duration").value));
    byID("scope-note").textContent = `${scoped.length} distinct cases / cohort: ${byID("cohort").value || "not populated"}. Repeated QPS values stay separate. Comparison filters do not change the fixed reference summary or selected case.`;
    const metrics = new Map();
    for (const run of scoped) for (const metric of list(run.latencies)) metrics.set(metric.id, metric);
    const charts = byID("latency-charts");
    charts.replaceChildren();
    for (const [id, metric] of metrics) {
      charts.append(distributionPanel(metric.label || id, scoped.map(run => {
        const entry = list(run.latencies).find(item => item.id === id);
        return { run, distribution: entry?.distribution, extra: entry || {} };
      }), metric.unit || "s"));
    }
    if (!metrics.size) charts.append(element("p", "No latency distributions supplied for this scope. Not tested/no completed samples.", "empty"));
    renderResources();
  }

  function renderResources() {
    const root = byID("resource-charts");
    root.replaceChildren();
    const query = byID("component-search").value.trim().toLowerCase();
    const ids = [...new Set([...list(size.components).map(component => component.id), ...scoped.flatMap(run => list(run.resources).map(usage => usage.componentID))])];
    const matches = ids.filter(id => {
      const aliases = /kube-apiserver|kube apiserver/i.test(`${id} ${componentName(id)}`) ? " kas " : "";
      return `${id} ${componentName(id)}${aliases}`.toLowerCase().includes(query);
    });
    byID("resource-count").textContent = `${matches.length} of ${ids.length} components / ${scoped.length} cases. Expand any row; none are ranked out or pooled.`;
    for (const id of matches) {
      const fold = element("details", undefined, "component-distribution");
      const summary = element("summary");
      summary.append(element("span", componentName(id)), element("small", id));
      fold.append(summary);
      // Build only when expanded; all components remain discoverable without
      // allocating thousands of SVG nodes for the collapsed catalog.
      fold.addEventListener("toggle", () => {
        if (!fold.open || fold.childElementCount > 1) return;
        const panels = element("div", undefined, "chart-grid");
        for (const [metric, title] of [["cpu", "CPU"], ["memory", "Memory / cgroup charge"]]) {
          panels.append(distributionPanel(title, scoped.map(run => {
            const usage = list(run.resources).find(item => item.componentID === id);
            return { run, distribution: usage?.[metric], request: usage?.[`${metric}Request`], limit: usage?.[`${metric}Limit`], caps: true };
          }), metric));
        }
        fold.append(panels);
      });
      root.append(fold);
    }
    if (!matches.length) root.append(element("p", "No components match this filter.", "empty"));
  }

  function renderComposition() {
    const root = byID("treemaps");
    root.replaceChildren();
    const p = percentile();
    const pLabel = percentileLabel();
    byID("percentile-output").textContent = pLabel;
    byID("percentile").setAttribute("aria-valuetext", pLabel);
    if (!selected) {
      root.append(element("p", "No measured run selected.", "empty"));
      return;
    }
    for (const metric of ["cpu", byID("memory-metric").value]) {
      const names = { cpu: "CPU", memory: "Memory / cgroup charge", workingSet: "Memory / working set", rss: "Memory / RSS" };
      const panel = element("section", undefined, "chart-panel");
      panel.append(element("h3", names[metric]));
      const usages = list(selected.resources);
      const ids = new Set([...list(size.components).map(component => component.id), ...usages.map(usage => usage.componentID)]);
      const available = usages.filter(usage => usage[metric]?.count > 0 && numeric(usage[metric][p]));
      const tiles = available.filter(usage => usage[metric][p] > 0).sort((a, b) => b[metric][p] - a[metric][p] || a.componentID.localeCompare(b.componentID));
      const total = tiles.reduce((sum, usage) => sum + usage[metric][p], 0);
      panel.append(element("p", `${unit(total, metric)} / Sum of component ${pLabel}s (not whole-HCP ${pLabel})`));
      const whole = list(selected.wholeHCP).find(item => item.id === (metric === "workingSet" ? "working-set" : metric));
      panel.append(element("p", `Whole-HCP ${pLabel}: ${whole?.distribution?.count > 0 ? unit(whole.distribution[p], metric) : "Unavailable"}. ${ids.size - available.length} components unavailable; ${available.length - tiles.length} measured zero.`));
      const map = element("div", undefined, "treemap");
      const legend = element("div", undefined, "component-legend");
      if (!tiles.length) map.append(element("p", "No positive measured component values for this run and percentile.", "empty"));

      // Recursive binary partition preserves area ratios. Stable component colors
      // and a complete keyboard-accessible legend preserve identity between runs.
      const split = (items, x, y, width, height) => {
        if (!items.length) return;
        if (items.length === 1) {
          const usage = items[0];
          const name = componentName(usage.componentID);
          const value = unit(usage[metric][p], metric);
          const button = element("button", undefined, "tile");
          button.type = "button";
          button.style.cssText = `left:${x}%;top:${y}%;width:${width}%;height:${height}%;background:${color(usage.componentID)}`;
          button.setAttribute("aria-label", `${name}: ${pLabel} ${value}. Open component details.`);
          button.setAttribute("aria-pressed", String(selectedComponent === usage.componentID));
          button.title = `${name}: ${pLabel} ${value}`;
          if (width >= 16 && height >= 13) button.append(element("strong", name), element("span", value));
          button.addEventListener("click", () => showComponent(usage.componentID, true));
          map.append(button);
          return;
        }
        const sum = items.reduce((value, usage) => value + usage[metric][p], 0);
        let amount = items[0][metric][p];
        let cut = 1;
        while (cut < items.length - 1 && amount + items[cut][metric][p] / 2 < sum / 2) amount += items[cut++][metric][p];
        const fraction = amount / sum;
        if (width >= height) {
          split(items.slice(0, cut), x, y, width * fraction, height);
          split(items.slice(cut), x + width * fraction, y, width * (1 - fraction), height);
        } else {
          split(items.slice(0, cut), x, y, width, height * fraction);
          split(items.slice(cut), x, y + height * fraction, width, height * (1 - fraction));
        }
      };
      split(tiles, 0, 0, 100, 100);
      for (const id of ids) {
        const usage = available.find(item => item.componentID === id);
        const entry = element("button");
        entry.type = "button";
        const swatch = element("span", undefined, "swatch");
        swatch.style.background = color(id);
        if (!usage) entry.classList.add("unavailable");
        entry.append(swatch, document.createTextNode(`${componentName(id)} ${usage ? unit(usage[metric][p], metric) : "Unavailable"}`));
        entry.addEventListener("click", () => showComponent(id, true));
        legend.append(entry);
      }
      panel.append(map, legend);
      root.append(panel);
    }
  }

  function showComponent(id, focus) {
    selectedComponent = id;
    const root = byID("component-detail");
    root.replaceChildren();
    const usage = list(selected?.resources).find(item => item.componentID === id);
    if (!usage) {
      root.append(element("p", id ? `${componentName(id)} / Unavailable for this run. No measured resource distribution or budget supplied.` : "Select a tile or legend entry for component requests, limits and coverage."));
      if (focus) root.focus();
      return;
    }
    root.append(element("h3", componentName(id)), element("p", `${id} / ${number(usage.replicas)} replicas / ${number(usage.containers)} containers / absolute component totals`));
    const facts = element("dl", undefined, "facts");
    for (const [label, value, metric] of [
      ["CPU request", usage.cpuRequest, "cpu"], ["CPU limit", usage.cpuLimit, "cpu"],
      ["Memory request", usage.memoryRequest, "memory"], ["Memory limit", usage.memoryLimit, "memory"],
      ["Go target", usage.goTarget, "memory"], ["Selected CPU", usage.cpu?.count > 0 ? usage.cpu[percentile()] : null, "cpu"],
      ["Selected memory", usage[byID("memory-metric").value]?.count > 0 ? usage[byID("memory-metric").value][percentile()] : null, "memory"]
    ]) {
      const fact = element("div");
      fact.append(element("dt", label), element("dd", unit(value, metric)));
      facts.append(fact);
    }
    root.append(facts);
    for (const [metric, request] of [["cpu", usage.cpuRequest], ["memory", usage.memoryRequest]]) {
      const value = usage[metric]?.count > 0 ? usage[metric][percentile()] : null;
      root.append(element("p", `${metric === "cpu" ? "CPU" : "Memory charge"} ${percentileLabel()} / request: ${numeric(value) && numeric(request) && request > 0 ? `${number(value / request * 100)}%` : "Unknown or no positive request"}. ${statistics(usage[metric], metric)}`));
    }
    for (const note of list(usage.notes)) root.append(element("p", note));
    renderComposition();
    if (focus) root.focus();
  }

  function renderSelected() {
    const register = runs();
    selectOptions(byID("selected-run"), register.map(run => [run.id, `${runLabel(run)} / ${run.label}`]), selected?.id);
    byID("selected-run").disabled = !register.length;
    const slider = byID("run-slider");
    slider.max = String(Math.max(0, register.length - 1));
    slider.value = String(Math.max(0, register.indexOf(selected)));
    slider.disabled = register.length < 2;
    slider.setAttribute("aria-valuetext", selected ? runLabel(selected) : "No measured runs");
    byID("run-output").textContent = selected ? runLabel(selected) : "No measured runs";
    byID("selected-title").textContent = selected?.label || "No measured run selected";
    byID("selected-context").textContent = selected ? `${selected.cohort} / ${runLabel(selected)}` : "";
    for (const id of ["selected-badges", "incidents", "request-outcomes", "run-notes", "run-metadata"]) byID(id).replaceChildren();
    if (selected) {
      const assessment = selected.assessment || {};
      for (const [key, label] of [["requestQuality", "Request quality"], ["health", "Health"], ["execution", "Execution"], ["capture", "Capture"]]) {
        const value = assessment[key] || "Unknown";
        const state = /^(pass|passed)$/i.test(value) ? "pass" : /^(fail|failed)$/i.test(value) ? "fail" : "neutral";
        byID("selected-badges").append(element("span", `${label}: ${value}`, `badge ${state}`));
      }
      for (const [label, value] of [["Hard canary failures", selected.canaryHardFailed], ["Soft degraded cycles", selected.canarySoftDegraded], ["Restarts", selected.restarts], ["OOMs", selected.ooms]]) {
        const cell = element("div");
        cell.append(element("strong", number(value)), element("span", label));
        byID("incidents").append(cell);
      }
      const requests = selected.requests || {};
      const measured = requests.dispatched > 0;
      const fraction = measured ? requests.degradedUnion / requests.dispatched : null;
      const outcomes = byID("request-outcomes");
      outcomes.append(element("p", `${measured ? `${number(fraction * 100)}%` : "Not measured:"} degraded request union / supplied request-quality assessment: ${assessment.requestQuality || "Unknown"}. ${numeric(requests.maxDegradedFraction) ? `Threshold ${number(requests.maxDegradedFraction * 100)}%` : "Legacy strict policy"}.`));
      if (measured && numeric(fraction)) {
        const meter = element("meter", `${number(fraction * 100)}% degraded`, "quality-meter");
        meter.min = 0;
        meter.max = 1;
        meter.value = fraction;
        meter.setAttribute("aria-label", "Fraction of dispatched requests in the degraded union; assessment shown separately");
        outcomes.append(meter);
      }
      outcomes.append(element("p", `Offered ${number(requests.offered)} / dispatched ${number(requests.dispatched)} / successful ${number(requests.successful)} / rejected 429 ${number(requests.rejected429)} / timeouts ${number(requests.timeouts)} / degraded union ${number(requests.degradedUnion)} / canceled ${number(requests.canceled)} / other errors ${number(requests.otherErrors)} / generator limited ${number(requests.generatorLimited)}.`));
      outcomes.append(element("p", `Successful throughput ${number(requests.successfulQPS)} QPS. ${requests.throughputBasis || "Throughput basis unavailable."} Request-quality and control-plane health are separate assessments; a passing request gate is not qualification.`));
      byID("run-notes").append(element("p", assessment.summary || "No assessment summary supplied."));
      const notes = element("ul");
      for (const note of list(selected.notes)) notes.append(element("li", note));
      byID("run-notes").append(notes);
      const metadata = element("dl", undefined, "facts");
      for (const [label, value] of [
        ["Run ID", selected.id], ["Cohort", selected.cohort], ["Evidence kind", selected.kind],
        ["Start", selected.start], ["End", selected.end], ["Planned / actual", `${duration(selected.plannedSeconds)} / ${duration(selected.actualSeconds)}`],
        ["Population", `${selected.population} / ${selected.applicationPods} pods / ${selected.applications} applications / ${selected.guestNodes} guest nodes`],
        ["Stability policy", selected.stabilityPolicy], ["Contended", String(selected.contended)],
        ["Turnover period", `${number(selected.turnoverPeriodSeconds)} s`], ["Rollouts completed / scheduled", `${selected.rolloutsCompleted} / ${selected.rolloutsScheduled}`],
        ["Canary cycles", number(selected.canaryCycles)], ["Capture", assessment.capture || "Unknown"]
      ]) {
        const item = element("div");
        item.append(element("dt", label), element("dd", value || "Unknown"));
        metadata.append(item);
      }
      byID("run-metadata").append(metadata);
      for (const id of list(selected.sources)) {
        const source = list(data.sources).find(item => item.id === id);
        byID("run-metadata").append(element("p", source ? `${source.label} / SHA-256 ${source.sha256 || "unavailable"}` : `${id} / source details unavailable`, "hash"));
      }
      if (!list(selected.sources).length) byID("run-metadata").append(element("p", "No source citations supplied for this run."));
    }
    for (const button of byID("run-points").children) button.setAttribute("aria-pressed", String(button.dataset.run === selected?.id));
    for (const point of byID("envelope-chart").querySelectorAll("[data-run]")) point.setAttribute("aria-pressed", String(point.dataset.run === selected?.id));
    byID("envelope-selection").textContent = selected ? `Selected: ${selected.label} / ${runLabel(selected)} / health: ${selected.assessment?.health || "Unknown"}` : "Select a measured case.";
    showComponent(selectedComponent, false);
    if (!selectedComponent || !list(selected?.resources).some(usage => usage.componentID === selectedComponent)) renderComposition();
  }

  function chooseRun(run, alignScope) {
    selected = run;
    if (alignScope) {
      byID("cohort").value = run.cohort;
      byID("kind").value = run.kind;
      byID("population").value = "compare";
      byID("duration").value = "all";
      renderComparisons();
    }
    renderSelected();
  }

  function activateTab(name, focus) {
    tab = name;
    for (const button of document.querySelectorAll('[role="tab"]')) {
      const active = button.id === `tab-${name}`;
      button.setAttribute("aria-selected", String(active));
      button.tabIndex = active ? 0 : -1;
      byID(button.getAttribute("aria-controls")).hidden = !active;
      if (active && focus) button.focus();
    }
  }

  function route() {
    let hash;
    try { hash = decodeURIComponent(location.hash.slice(1)); } catch { hash = ""; }
    const destination = hash || data.sizes[0].id;
    const next = data.sizes.find(item => item.id === destination);
    for (const section of document.querySelectorAll(".size-summary")) section.hidden = section.dataset.size !== destination;
    for (const link of document.querySelectorAll("#navigation a")) {
      if (decodeURIComponent(link.hash.slice(1)) === destination) link.setAttribute("aria-current", "page");
      else link.removeAttribute("aria-current");
    }
    byID("placeholder").hidden = Boolean(next);
    byID("workspace").hidden = !next;
    if (!next) {
      const link = [...document.querySelectorAll("#navigation a")].find(item => decodeURIComponent(item.hash.slice(1)) === destination);
      byID("placeholder-title").textContent = link?.dataset.doc || link?.dataset.placeholder || "Not populated";
      byID("placeholder-copy").textContent = link?.dataset.doc ? "This documentation page is reserved for a later edition." : "Not populated. No measured results have been supplied for this size.";
    } else if (size !== next) {
      size = next;
      selectedComponent = undefined;
      selected = runs().find(run => run.id === size.defaultRun) || runs()[0];
      const cohorts = [...new Set(runs().map(run => run.cohort))];
      const pinned = cohorts.find(cohort => /pinned.*envelope/.test(cohort) && runs().some(run => run.cohort === cohort && run.kind === "screen")) || cohorts[0];
      selectOptions(byID("cohort"), cohorts.map(cohort => [cohort, cohort]), pinned);
      byID("kind").value = "screen";
      byID("population").value = "compare";
      selectOptions(byID("duration"), [["all", "All durations"], ...[...new Set(runs().map(run => run.plannedSeconds))].sort((a, b) => a - b).map(seconds => [String(seconds), duration(seconds)])], "all");
      byID("run-points").replaceChildren();
      for (const run of runs()) {
        const point = element("button", runLabel(run), run.population === "unloaded" ? "unloaded" : "loaded");
        point.type = "button";
        point.dataset.run = run.id;
        point.title = `${run.label} / ${run.cohort} / ${run.stabilityPolicy}`;
        point.addEventListener("click", () => chooseRun(run, true));
        byID("run-points").append(point);
      }
      byID("component-search").value = "";
      renderEnvelope();
      renderComparisons();
      renderSelected();
    }
    byID("navigation").classList.remove("expanded");
    byID("nav-toggle").setAttribute("aria-expanded", "false");
    activateTab(tab, false);
  }

  for (const id of ["cohort", "kind", "population", "duration"]) byID(id).addEventListener("change", renderComparisons);
  byID("selected-run").addEventListener("change", event => chooseRun(runs().find(run => run.id === event.target.value), false));
  byID("run-slider").addEventListener("input", event => chooseRun(runs()[Number(event.target.value)], false));
  for (const id of ["percentile", "memory-metric"]) byID(id).addEventListener("input", () => {
    showComponent(selectedComponent, false);
    if (!selectedComponent || !list(selected?.resources).some(usage => usage.componentID === selectedComponent)) renderComposition();
  });
  byID("component-search").addEventListener("input", renderResources);
  const tabs = [...document.querySelectorAll('[role="tab"]')];
  for (const [index, button] of tabs.entries()) {
    button.addEventListener("click", () => activateTab(button.id.slice(4), false));
    button.addEventListener("keydown", event => {
      let next;
      if (event.key === "ArrowRight") next = (index + 1) % tabs.length;
      if (event.key === "ArrowLeft") next = (index + tabs.length - 1) % tabs.length;
      if (event.key === "Home") next = 0;
      if (event.key === "End") next = tabs.length - 1;
      if (next !== undefined) {
        event.preventDefault();
        activateTab(tabs[next].id.slice(4), true);
      }
    });
  }
  byID("nav-toggle").hidden = false;
  byID("nav-toggle").addEventListener("click", () => {
    const expanded = byID("navigation").classList.toggle("expanded");
    byID("nav-toggle").setAttribute("aria-expanded", String(expanded));
  });
  byID("navigation").addEventListener("keydown", event => {
    if (event.key === "Escape") {
      byID("navigation").classList.remove("expanded");
      byID("nav-toggle").setAttribute("aria-expanded", "false");
      byID("nav-toggle").focus();
    }
  });
  for (const register of document.querySelectorAll(".run-register")) register.open = false;
  document.documentElement.classList.add("js");
  window.addEventListener("hashchange", route);
  route();
})();
