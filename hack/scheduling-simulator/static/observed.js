/* Normalize measured inventory for the shared tetris renderer. No inferred placement. */
(() => {
  'use strict';
  const source = document.getElementById('observed-data');
  if (!source) return;
  const list = value => Array.isArray(value) ? value : [];
  const known = value => typeof value === 'number' && Number.isFinite(value) && value >= 0;
  const text = value => value == null || value === '' ? 'Unknown' : String(value);
  try {
    const view = JSON.parse(source.textContent);
    if (view == null) return;
    if (view.schema_version !== 1 || view.mode !== 'observed' || !Array.isArray(view.management_clusters)) {
      throw new Error('Expected observed view schema version 1');
    }
    const regional = Array.isArray(view.regional_windows);
    let nextID = 1;
    const identities = new Map();
    const id = (...parts) => {
      const key = JSON.stringify(parts);
      if (!identities.has(key)) identities.set(key, nextID++);
      return identities.get(key);
    };
    // Number namespace buckets independently of node/pod traversal. Stable across
    // basis/lens switches and reordered copies of the same snapshot.
    const hcpIDs = new Map();
    for (const mc of [...view.management_clusters].sort((a, b) => a.id.localeCompare(b.id))) {
      for (const namespace of [...new Set(podsForMC(mc).map(p => p.namespace)
        .filter(ns => ns?.startsWith('ocm-arohcp')))].sort()) {
        hcpIDs.set(JSON.stringify([mc.id, namespace]), hcpIDs.size + 1);
      }
    }
    function componentName(pod) {
      const value = pod.component || pod.name;
      // Prefer resolved owner names. ReplicaSet hashes may remain when its
      // Deployment owner metric is unavailable.
      if (value !== pod.name) return value.replace(/-[bcdfghjklmnpqrstvwxz2456789]{8,10}$/, '');
      return value.replace(/-[bcdfghjklmnpqrstvwxz2456789]{8,10}-[a-z0-9]{5}$/, '')
        .replace(/-[0-9]+$/, '').replace(/-[a-z0-9]{5}$/, '');
    }
    const issues = [...list(view.errors), ...list(view.warnings)];
    const pods = [...new Map(view.management_clusters.flatMap(mc => podsForMC(mc)
      .map(p => [JSON.stringify([mc.id, p.id ?? [p.namespace, p.name]]), p]))).values()];
    for (const mc of view.management_clusters) {
      issues.push(...list(mc.warnings).map(w => `${text(mc.name)}: ${w}`));
      if (list(mc.unplaced_pods).length) issues.push(`${text(mc.name)}: ${mc.unplaced_pods.length} unplaced pods (HCP lens only; no synthetic node).`);
    }
    const diagnosticCount = issues.length;
    const incomplete = pods.filter(p => !known(p.usage?.cpu_mc) || !known(p.usage?.mem_mib)
      || !(p.coverage?.cpu >= 1) || !(p.coverage?.memory >= 1));
    const pending = incomplete.filter(p => p.current === true && p.phase === 'pending').length;
    const current = incomplete.filter(p => p.current === true).length;
    const breakdown = [pending && `${pending} Pending`,
      current > pending && `${current - pending} current pods with unavailable usage`,
      incomplete.length > current && `${incomplete.length - current} historical`].filter(Boolean).join(', ');
    for (const [resource, coverage] of [['cpu_mc', 'cpu'], ['mem_mib', 'memory']]) {
      const complete = pods.filter(p => known(p.usage?.[resource]) && p.coverage?.[coverage] >= 1).length;
      issues.push(`${resource}: ${complete}/${pods.length} pods with complete window usage coverage.`);
    }
    const nodes = view.management_clusters.flatMap(mc => list(mc.nodes));
    issues.push(`${nodes.filter(n => !known(n.capacity?.cpu_mc) || !known(n.capacity?.mem_mib)).length}/${nodes.length} nodes with unknown CPU/memory capacity.`);
    issues.push(`${nodes.filter(n => !known(n.node_usage?.cpu_mc) || !known(n.node_usage?.mem_mib)).length}/${nodes.length} nodes with missing independent node usage.`);
    issues.push(`${nodes.filter(n => !known(n.capacity?.nic)).length}/${nodes.length} nodes with unknown NIC capacity.`);
    const summary = document.getElementById('obs-summary');
    summary.textContent = (regional ? 'Regional peak snapshots — different times; not simultaneous fleet load / '
      : `T: ${text(view.at)} / window: ${text(view.start)} to T / `)
      + `${pods.length} pods: ${pods.length - incomplete.length} complete usage; `
      + `${pods.filter(p => p.current === true).length} current, ${pods.filter(p => p.current !== true).length} historical / `
      + `${incomplete.length} pods with incomplete usage${breakdown ? `: ${breakdown}` : ''} / ${diagnosticCount} diagnostics & provenance`;
    const details = document.getElementById('obs-provenance');
    if (regional || view.peak_selection?.original_selected_at != null) {
      const windows = regional ? view.regional_windows : [{
        environment: view.peak_selection.environment, region: view.peak_selection.region,
        at: view.at, start: view.start, peak_selection: view.peak_selection,
      }];
      const explanation = document.createElement('p');
      explanation.textContent = 'Original peak scores rank regional window-average absolute consumption, not utilization. '
        + 'They describe the original peak windows, not the chosen shifted windows; shifted windows are not re-scored. '
        + 'Each lane uses its own chosen window; T means that lane\'s window end.';
      details.append(explanation);
      const wrap = document.createElement('div');
      wrap.className = 'cost-table-wrap';
      const table = document.createElement('table');
      table.className = 'tight';
      const caption = document.createElement('caption');
      caption.textContent = 'Regional peak windows';
      table.append(caption);
      const header = document.createElement('tr');
      for (const label of ['Environment / region', 'Original peak window (UTC)', 'Chosen window (UTC)', 'Original peak score', 'Adjustment / reason']) {
        const cell = document.createElement('th');
        cell.scope = 'col';
        cell.textContent = label;
        header.append(cell);
      }
      const thead = document.createElement('thead');
      thead.append(header);
      table.append(thead);
      const body = document.createElement('tbody');
      const iso = epoch => known(epoch) ? new Date(epoch * 1000).toISOString() : 'Unknown';
      for (const entry of windows) {
        const peak = entry.peak_selection || {};
        const memory = peak.metric === 'memory';
        const score = known(peak.score) ? `${(peak.score / (memory ? 2 ** 30 : 1)).toFixed(2)} ${memory ? 'GiB' : 'cores'} (${text(peak.metric)})` : 'Unavailable';
        const reason = entry.reason || (peak.adjustment_seconds > 0
          ? `Shifted forward ${peak.adjustment_seconds}s for HCP size settling (${text(peak.settle_seconds)}s).`
          : entry.at ? 'Unchanged (no forward shift needed).' : 'Not collected.');
        const row = document.createElement('tr');
        for (const value of [`${text(entry.environment)} / ${text(entry.region)}`,
          `${iso(peak.original_start ?? (peak.original_selected_at - view.window_seconds))} to ${iso(peak.original_selected_at)}`,
          entry.at ? `${text(entry.start)} to ${text(entry.at)}` : 'Not collected', score, reason]) {
          const cell = document.createElement('td');
          cell.textContent = value;
          row.append(cell);
        }
        body.append(row);
      }
      table.append(body);
      wrap.append(table);
      details.append(wrap);
      issues.push('Regional windows:', JSON.stringify(windows, null, 2));
    } else if (view.peak_selection && view.peak_selection.scope !== 'regional') {
      const peak = view.peak_selection;
      const memory = peak.metric === 'memory';
      const score = peak.score / (memory ? 2 ** 30 : 1);
      const notice = document.createElement('p');
      notice.className = 'muted';
      notice.textContent = `Peak-selected snapshot: highest ${view.window_seconds / 60}-minute average fleet ${peak.metric} over ${(peak.search_end - peak.search_start) / 86400} days. ${score.toFixed(2)} ${memory ? 'GiB' : 'cores'} across ${peak.clusters.length} MCs. Absolute consumption, not utilization percentage.`;
      details.after(notice);
      issues.push('Peak selection:', JSON.stringify(peak, null, 2));
    }
    details.open = false;
    const pre = document.createElement('pre');
    pre.textContent = [`Generated: ${text(view.generated_at)}; sample step: ${text(view.step_seconds)}s; window: ${text(view.window_seconds)}s`,
      ...issues, 'Sources:', JSON.stringify(view.sources || [], null, 2),
      'Transitions:', JSON.stringify(view.transitions || [], null, 2),
      'Suggested window (not applied):', JSON.stringify(view.suggestion || null, null, 2)].join('\n');
    details.append(pre);

    const payload = { mode: 'observed', regional };
    for (const basis of ['usage', 'requests']) {
      payload[basis] = { management_clusters: view.management_clusters.map((mc, mi) => {
        const metadata = new Map(list(mc.hcps).map(hcp => [hcp.id, hcp]));
        const normalizePod = (pod, node) => {
          const hcp = metadata.get(pod.hcp_id);
          const shortID = hcpIDs.get(JSON.stringify([mc.id, pod.namespace]));
          const isHcp = shortID != null;
          const p = {
            id: id('pod', mi, node?.id, pod.id, pod.namespace, pod.name),
            hcp: isHcp ? shortID : hcpIDs.size + id('namespace', mc.id, pod.namespace),
            hcp_size: isHcp ? text(hcp?.size || pod.hcp_size) : 'Non-HCP',
            category: isHcp ? 'HCP' : text(pod.category || 'Non-HCP'),
            component: componentName(pod), tier: text(node?.pool),
            label: isHcp ? `HCP ${shortID}` : text(pod.namespace),
            observed: { ...pod, hcp, node: node?.name, nodeID: node ? id('node', mi, node.id, node.name) : null },
          };
          for (const resource of ['cpu_mc', 'mem_mib', 'nic', 'pods']) {
            const value = resource === 'pods' ? (pod.current === true ? 1 : 0)
              : resource === 'nic' ? (pod.current === true ? pod.requests?.nic : 0)
              : pod[basis]?.[resource];
            p[resource] = known(value) ? value : null;
          }
          return p;
        };
        const groups = new Map();
        for (const node of list(mc.nodes)) {
          const normalized = {
            id: id('node', mi, node.id, node.name), sku: text(node.sku), observed: node,
            full: {}, infra: { system: {}, daemonset: {}, buffer: {} }, used: {}, unknown: {}, incomplete: {},
            pods: list(node.pods).filter(p => basis === 'usage' || p.current === true).map(p => normalizePod(p, node)),
          };
          for (const resource of ['cpu_mc', 'mem_mib', 'nic', 'pods']) {
            const cap = node.capacity?.[resource], alloc = node.allocatable?.[resource];
            const currentResource = basis === 'requests' || resource === 'nic' || resource === 'pods';
            normalized.full[resource] = known(cap) ? cap : null;
            normalized.infra.system[resource] = currentResource && node.current === true && known(cap) && known(alloc) && alloc <= cap ? cap - alloc : 0;
            normalized.used[resource] = normalized.pods.reduce((sum, p) => sum + (p[resource] ?? 0), 0);
            normalized.incomplete[resource] = normalized.pods.filter(p => p[resource] == null
              || (!currentResource && !(p.observed.coverage?.[resource === 'cpu_mc' ? 'cpu' : 'memory'] >= 1))).length;
            normalized.unknown[resource] = node.current !== true || !known(cap)
              || (currentResource && (!known(alloc) || alloc > cap))
              || normalized.incomplete[resource] > 0;
          }
          const group = JSON.stringify([node.pool, node.zone]);
          const zone = mc.region && node.zone?.startsWith(`${mc.region}-`) ? node.zone.slice(mc.region.length + 1) : node.zone;
          if (!groups.has(group)) groups.set(group, {
            title: `Pool: ${text(node.pool)} / Zone: ${text(zone)}`, nodes: [], az: groups.size,
            layoutBand: /^(infra|system)/.test(node.pool || '') ? 'platform' : node.pool ? 'workers' : 'unknown',
          });
          groups.get(group).nodes.push(normalized);
        }
        return {
          id: id('mc', mi), name: text(mc.name), observed: mc, count: 1,
          hcps: new Set(podsForMC(mc).filter(p => p.namespace?.startsWith('ocm-arohcp')).map(p => p.namespace)).size,
          packing: { groups: [...groups.values()],
            unplaced: list(mc.unplaced_pods).filter(p => basis === 'usage' || p.current === true).map(p => normalizePod(p, null)) },
        };
      }) };
    }
    function podsForMC(mc) { return [...list(mc.nodes).flatMap(n => list(n.pods)), ...list(mc.unplaced_pods)]; }
    document.getElementById('sim-data').textContent = JSON.stringify(payload);
  } catch (error) {
    const notice = document.getElementById('obs-runtime-error');
    notice.hidden = false;
    notice.textContent = `Observed snapshot could not be rendered: ${error.message}`;
    document.getElementById('viz')?.setAttribute('hidden', '');
  }
})();
