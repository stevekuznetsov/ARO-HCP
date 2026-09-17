'use strict';

const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const { test } = require('node:test');
const { Script, createContext } = require('node:vm');

const filename = join(__dirname, '../static/observed.js');
const adapter = new Script(readFileSync(filename, 'utf8'), { filename });

function adapt(view, output = {}) {
  const elements = {
    'observed-data': { textContent: JSON.stringify(view) },
    'sim-data': { textContent: '' },
    'obs-summary': { textContent: '' },
    'obs-provenance': { open: true, children: [], append(child) { this.children.push(child); }, after(child) { this.notice = child; } },
    'obs-runtime-error': { hidden: true, textContent: '' },
    viz: { setAttribute() {} },
  };
  adapter.runInNewContext({
    document: {
      getElementById: id => elements[id] ?? null,
      createElement: tagName => ({ tagName, textContent: '', children: [],
        append(child) { this.children.push(child); },
        set innerHTML(_) { assert.fail('adapter must use safe DOM text, not HTML'); } }),
    },
  }, { timeout: 1000 });
  assert.equal(elements['obs-runtime-error'].hidden, true,
    elements['obs-runtime-error'].textContent);
  assert.notEqual(elements['sim-data'].textContent, '', 'adapter must publish #sim-data');
  Object.assign(output, elements);
  return JSON.parse(elements['sim-data'].textContent);
}

const rendererFilename = join(__dirname, '../static/tetris.js');
const renderer = new Script(readFileSync(rendererFilename, 'utf8'), { filename: rendererFilename });
function render(data, policy = 'usage') {
  const elements = { legend: {}, side: {}, tooltip: { style: {} } };
  const labels = [], textDraws = [];
  const context = createContext({
    data, policy, labels, textDraws, setTimeout, clearTimeout,
    document: { readyState: 'loading', addEventListener() {}, getElementById: id => elements[id] },
    window: { innerWidth: 1200, innerHeight: 800 },
  });
  renderer.runInContext(context);
  const run = code => new Script(code).runInContext(context, { timeout: 1000 });
  run(`DATA = data; S.policy = policy; S.resource = 'cpu';
    ctx = { fillRect() {}, strokeRect() {}, beginPath() {}, moveTo() {}, lineTo() {}, stroke() {},
      measureText(s) { return { width: s.length }; }, fillText(s, x, y) { labels.push(s); textDraws.push({ text: s, x, y }); } };
    canvas = { style: {}, getBoundingClientRect() { return { left: 0, top: 0 }; } };`);
  return { run, elements, labels, textDraws };
}

// All inventory is synthetic, with no telemetry files or network dependencies.
function pod(name, namespace = 'ocm-arohcp-test-alpha', overrides = {}) {
  return {
    id: `${namespace}/${name}`, name, namespace, component: name,
    current: true, hcp_id: null, hcp_size: null,
    usage: { cpu_mc: 10, mem_mib: 20 },
    requests: { cpu_mc: 100, mem_mib: 200, nic: 0 },
    coverage: { cpu: 1, memory: 1 },
    ...overrides,
  };
}

function node(id, pods, overrides = {}) {
  return {
    id, name: id, current: true, pool: 'test-pool', zone: 'test-zone',
    sku: 'test-sku', pods,
    capacity: { cpu_mc: 16000, mem_mib: 65536, pods: 225, nic: 64 },
    allocatable: { cpu_mc: 15000, mem_mib: 64000, pods: 225, nic: 64 },
    node_usage: { cpu_mc: 1000, mem_mib: 2000 },
    ...overrides,
  };
}

function mc(id, nodes, overrides = {}) {
  return { id, name: id, nodes, hcps: [], unplaced_pods: [], ...overrides };
}

function snapshot(management_clusters, overrides = {}) {
  return { schema_version: 1, mode: 'observed', management_clusters, ...overrides };
}

function nodes(cluster) {
  return cluster.packing.groups.flatMap(group => group.nodes);
}

function pods(cluster) {
  return [...nodes(cluster).flatMap(n => n.pods), ...cluster.packing.unplaced];
}

test('HCP namespace IDs are sequential and stable across traversal order and basis', () => {
  const alpha = 'ocm-arohcp-test-alpha', controlPlane = `${alpha}-control-plane`;
  const beta = 'ocm-arohcp-test-beta';
  const view = snapshot([
    mc('mc-z', [node('z-2', [pod('z-beta', beta)]), node('z-1', [pod('z-alpha', alpha)])]),
    mc('mc-a', [
      node('a-2', [pod('a-control', controlPlane, { hcp_id: 'hc-a' }), pod('system', 'kube-system')]),
      node('a-1', [pod('a-alpha-2', alpha), pod('a-alpha-1', alpha, { hcp_id: 'hc-a' })]),
    ], {
      hcps: [{ id: 'hc-a', size: 'small' }, { id: 'unused-metadata', size: 'large' }],
      unplaced_pods: [pod('pending-beta', beta), pod('pending-alpha', alpha)],
    }),
  ]);
  const reordered = structuredClone(view);
  reordered.management_clusters.reverse();
  for (const cluster of reordered.management_clusters) {
    cluster.nodes.reverse();
    cluster.hcps.reverse();
    cluster.unplaced_pods.reverse();
    for (const n of cluster.nodes) n.pods.reverse();
  }
  const expected = [
    ['mc-a', alpha, 1], ['mc-a', controlPlane, 2], ['mc-a', beta, 3],
    ['mc-z', alpha, 4], ['mc-z', beta, 5],
  ];
  for (const input of [view, reordered]) {
    const data = adapt(input);
    for (const basis of ['usage', 'requests']) {
      const actual = new Map();
      for (const cluster of data[basis].management_clusters) {
        for (const p of pods(cluster).filter(p => p.category === 'HCP')) {
          const key = JSON.stringify([cluster.name, p.observed.namespace]);
          if (actual.has(key)) assert.equal(p.hcp, actual.get(key), 'one ID per namespace');
          actual.set(key, p.hcp);
          assert.equal(p.label, `HCP ${p.hcp}`);
        }
        assert.equal(cluster.hcps, cluster.name === 'mc-a' ? 3 : 2);
      }
      assert.deepEqual([...actual].sort(), expected.map(([cluster, ns, id]) =>
        [JSON.stringify([cluster, ns]), id]).sort());
    }
  }
});

test('ocm-arohcp namespaces remain HCP without HostedCluster metadata', () => {
  const data = adapt(snapshot([mc('mc-a', [node('node-a', [pod('api', undefined, {
    hcp_id: 'missing', category: 'Non-HCP',
  })])])]));
  for (const basis of ['usage', 'requests']) {
    const cluster = data[basis].management_clusters[0];
    const [p] = pods(cluster);
    assert.equal(cluster.hcps, 1);
    assert.equal(p.hcp, 1);
    assert.equal(p.label, 'HCP 1');
    assert.equal(p.category, 'HCP');
    assert.equal(p.hcp_size, 'Unknown');
    assert.equal(p.observed.hcp_id, 'missing');
  }
});

test('Non-HCP namespaces have separate, MC-scoped IDs outside the HCP range', () => {
  const data = adapt(snapshot([
    mc('mc-a', [node('node-a', [
      pod('system-1', 'kube-system'), pod('hcp'), pod('system-2', 'kube-system'),
      pod('monitor', 'monitoring'), pod('other-ocm', 'ocm-unrelated'),
    ])]),
    mc('mc-b', [node('node-b', [pod('system-3', 'kube-system')])]),
  ]));
  const identities = [];
  for (const basis of ['usage', 'requests']) {
    const byNamespace = new Map();
    for (const cluster of data[basis].management_clusters) {
      for (const p of pods(cluster)) {
        if (p.category === 'HCP') { assert.equal(p.hcp, 1); continue; }
        assert.equal(p.category, 'Non-HCP');
        assert.equal(p.hcp_size, 'Non-HCP');
        assert.equal(p.label, p.observed.namespace);
        assert.ok(p.hcp > 1, 'Non-HCP IDs cannot collide with HCP IDs');
        const key = JSON.stringify([cluster.name, p.observed.namespace]);
        if (byNamespace.has(key)) assert.equal(p.hcp, byNamespace.get(key));
        byNamespace.set(key, p.hcp);
      }
    }
    assert.equal(byNamespace.size, 4);
    assert.equal(new Set(byNamespace.values()).size, 4);
    identities.push([...byNamespace]);
  }
  assert.deepEqual(identities[0], identities[1]);
});

for (const [description, name, component, expected] of [
  ['Deployment Kubernetes hash (not just hex)', 'kube-apiserver-7c9f6d8bdf-z8k2m', undefined, 'kube-apiserver'],
  ['StatefulSet ordinal', 'etcd-12', 'etcd-12', 'etcd'],
  ['DaemonSet random suffix', 'node-exporter-z8k2m', 'node-exporter-z8k2m', 'node-exporter'],
  ['resolved owner', 'api-7c9f6d8bdf-z8k2m', 'api', 'api'],
  ['resolved owner ending in five letters', 'network-agent-z8k2m', 'network-agent', 'network-agent'],
  ['resolved owner ending in an ordinal', 'controller-12-0', 'controller-12', 'controller-12'],
  ['resolved owner ending in eight letters', 'metrics-operator-7c9f6d8bdf-z8k2m', 'metrics-operator', 'metrics-operator'],
]) {
  test(`component names: ${description}`, () => {
    const data = adapt(snapshot([mc('mc-a', [node('node-a', [pod(name, undefined, { component })])])]));
    for (const basis of ['usage', 'requests']) {
      assert.equal(pods(data[basis].management_clusters[0])[0].component, expected);
    }
  });
}

test('53 current pods on a 225/225 node: only unknown NIC sets an unknown flag', () => {
  const n = node('node-a', Array.from({ length: 53 }, (_, i) => pod(`api-${i}`)));
  n.capacity.nic = null;
  n.allocatable.nic = null;
  const view = snapshot([mc('mc-a', [n])]);
  const withWarnings = structuredClone(view);
  withWarnings.errors = ['Unrelated source unavailable'];
  withWarnings.warnings = ['HostedCluster metadata missing'];
  withWarnings.management_clusters[0].warnings = ['Unrelated metadata warning'];
  withWarnings.management_clusters[0].nodes[0].warnings = ['Unknown NIC capacity'];
  for (const input of [view, withWarnings]) {
    const data = adapt(input);
    for (const basis of ['usage', 'requests']) {
      const [normalized] = nodes(data[basis].management_clusters[0]);
      assert.deepEqual(normalized.unknown, { cpu_mc: false, mem_mib: false, nic: true, pods: false });
      assert.equal(normalized.full.pods, 225);
      assert.equal(normalized.observed.allocatable.pods, 225);
      assert.equal(normalized.infra.system.pods, 0);
      assert.equal(normalized.used.pods, 53);
      assert.equal(normalized.full.pods - normalized.used.pods, 172);
      assert.equal(normalized.full.nic, null);
    }
  }
});

for (const [resource, coverage] of [['cpu_mc', 'cpu'], ['mem_mib', 'memory']]) {
  test(`${resource} coverage gap affects only that resource on that node in usage mode`, () => {
    const incomplete = pod('incomplete');
    incomplete.coverage[coverage] = 0.5;
    const data = adapt(snapshot([mc('mc-a', [
      node('incomplete-node', [incomplete]), node('complete-node', [pod('complete')]),
    ])]));
    const completeFlags = { cpu_mc: false, mem_mib: false, nic: false, pods: false };
    const [gap, complete] = nodes(data.usage.management_clusters[0]);
    assert.deepEqual(gap.unknown, { ...completeFlags, [resource]: true });
    assert.deepEqual(complete.unknown, completeFlags);
    for (const n of nodes(data.requests.management_clusters[0])) assert.deepEqual(n.unknown, completeFlags);
  });
}

test('historical pods contribute usage but not requests, NIC or current pod counts', () => {
  const historical = { current: false, requests: { cpu_mc: 9000, mem_mib: 9000, nic: 9 } };
  const current = { requests: { cpu_mc: 100, mem_mib: 200, nic: 1 } };
  const data = adapt(snapshot([mc('mc-a', [
    node('current-node', [pod('current', undefined, current), pod('departed', undefined, historical)]),
    node('retired-node', [pod('retired', undefined, historical)], { current: false }),
  ], { unplaced_pods: [pod('pending', undefined, current), pod('old-pending', undefined, historical)] })]));
  const usage = data.usage.management_clusters[0], requests = data.requests.management_clusters[0];
  assert.equal(pods(usage).length, 5);
  assert.deepEqual(pods(requests).map(p => p.observed.name), ['current', 'pending']);
  for (const p of pods(usage).filter(p => !p.observed.current)) {
    assert.equal(p.cpu_mc, 10);
    assert.equal(p.mem_mib, 20);
    assert.equal(p.pods, 0);
    assert.equal(p.nic, 0);
  }
  assert.deepEqual(nodes(usage).map(n => n.used), [
    { cpu_mc: 20, mem_mib: 40, nic: 1, pods: 1 },
    { cpu_mc: 10, mem_mib: 20, nic: 0, pods: 0 },
  ]);
  assert.deepEqual(nodes(requests).map(n => n.used), [
    { cpu_mc: 100, mem_mib: 200, nic: 1, pods: 1 },
    { cpu_mc: 0, mem_mib: 0, nic: 0, pods: 0 },
  ]);
  assert.deepEqual(nodes(requests)[1].infra.system, { cpu_mc: 0, mem_mib: 0, nic: 0, pods: 0 });
  for (const cluster of [usage, requests]) {
    assert.equal(pods(cluster).reduce((sum, p) => sum + p.pods, 0), 2);
    assert.equal(pods(cluster).reduce((sum, p) => sum + p.nic, 0), 2);
    assert.equal(cluster.packing.unplaced.find(p => p.observed.name === 'pending').observed.nodeID, null);
  }
});

test('provenance counts unique pods rather than diagnostics, with all diagnostics collapsed', () => {
  const complete = Array.from({ length: 665 }, (_, i) => pod(`complete-${i}`));
  const pending = Array.from({ length: 6 }, (_, i) => pod(`pending-${i}`, undefined, {
    phase: 'pending', usage_issues: ['pending'], usage: {}, coverage: {},
  }));
  const historical = Array.from({ length: 30 }, (_, i) => pod(`velero-${i}`, 'velero', {
    current: false, phase: 'succeeded', usage_issues: ['sampling-gap'], coverage: { cpu: 0.5, memory: 0.5 },
  }));
  const errors = Array.from({ length: 107 }, (_, i) => `Diagnostic ${i}`);
  const elements = {};
  const view = snapshot([mc('mc-a', [node('node-a', [...complete, ...pending, ...historical])])], { errors });
  adapt(view, elements);
  const summary = elements['obs-summary'].textContent;
  assert.match(summary, /701 pods: 665 complete usage; 671 current, 30 historical/);
  assert.match(summary, /36 pods with incomplete usage: 6 Pending, 30 historical/);
  assert.match(summary, /107 diagnostics & provenance/);
  assert.doesNotMatch(summary, /107 errors/);
  assert.equal(elements['obs-provenance'].open, false);
  const details = elements['obs-provenance'].children[0].textContent.split('\n');
  assert.deepEqual(details.filter(line => line.startsWith('Diagnostic ')), errors);
  view.management_clusters[0].unplaced_pods.push(pending[0]);
  adapt(view, elements);
  assert.equal(elements['obs-summary'].textContent, summary.replace('107 diagnostics', '108 diagnostics'),
    'repeated inventory must not inflate pod counts; only the unplaced-pod diagnostic is added');
});

test('provenance fallback never infers Pending from pod names, placement or issue hints', () => {
  const elements = {};
  adapt(snapshot([mc('mc-a', [], { unplaced_pods: [pod('pending', undefined, {
    usage: {}, coverage: {}, usage_issues: ['pending'],
  })] })]), elements);
  assert.match(elements['obs-summary'].textContent, /1 pods with incomplete usage: 1 current pods with unavailable usage/);
  assert.doesNotMatch(elements['obs-summary'].textContent, /Pending/);
});

for (const res of ['cpu', 'mem']) {
  test(`${res}: incomplete pod markers never hatch the known-capacity usage remainder in either lens`, () => {
    const data = adapt(snapshot([mc('mc-a', [node('node-a', [
      pod('complete'), pod('missing', undefined, { phase: 'pending', usage: {}, coverage: {} }),
      pod('partial', undefined, { coverage: { cpu: 0.5, memory: 0.5 }, usage_issues: ['sampling-gap'] }),
    ])]) ]));
    const { run, labels } = render(data);
    run(`S.resource = '${res}'; const n = allNodes(curMCs()[0].packing)[0];
      drawNodeTile(n, 0, 0, tileDims(n), 0);`);
    assert.equal(run(`remainderKind(n, S.resource)`), 'outside-pods');
    assert.equal(run(`n.incomplete[key(S.resource)]`), 2);
    assert.equal(run(`n.used[key(S.resource)]`), res === 'cpu' ? 20 : 40);
    assert.equal(run(`n.infra.system[key(S.resource)]`), 0, 'host usage must not be added to pod usage');
    assert.equal(run(`tiles[0].segMap.at(-1).seg.color`), '#1b2434');
    assert.equal(run(`tiles[0].segMap.at(-1).seg.hatch`), false);
    assert.equal(run(`tiles[0].segMap.filter(s => s.seg.hatch).length`), 2);
    assert.match(labels[0], /^\* node-a$/);
    assert.equal(run(`tiles[0].segMap.find(s => s.seg.pod?.observed.name === 'missing').seg.cells`), 1);
    run(`const ld = laneHcpData({ mc: curMCs()[0] }, S.resource);`);
    assert.equal(run('ld.unknown'), 0);
    assert.equal(run('ld.outsidePods'), (res === 'cpu' ? 16000 - 20 : 65536 - 40));
    run(`drawHcpCard({ hcp: n.pods[0].hcp, pods: n.pods }, 0, 0, 2);`);
    assert.equal(run('tiles[1].segMap.filter(s => s.seg.hatch).length'), 2);
    assert.equal(run(`tiles[1].segMap.find(s => s.seg.pod.observed.name === 'missing').seg.cells`), 1);
    const donut = run('donutSVG(n, S.resource)');
    assert.match(donut, /known pod total \(lower bound\)/);
    assert.match(donut, /stroke="#1b2434"/);
    assert.doesNotMatch(donut, /stroke="#70634b"/);
    run(`const bands = []; const originalDrawBand = drawBand;
      drawBand = (label, segs, ...args) => { bands.push({ label, segs }); return originalDrawBand(label, segs, ...args); };
      drawLaneHcp(ld, 0, 800, 4, 100, 50, 2, 96, 8, S.resource);`);
    assert.equal(run(`bands.at(-1).segs.filter(s => s.cells > 0).length`), 1);
    assert.equal(run(`bands.at(-1).segs.find(s => s.cells > 0).kind`), 'outside-pods');
    assert.equal(run(`bands.at(-1).segs.find(s => s.cells > 0).color`), '#1b2434');
    assert.equal(run(`Boolean(bands.at(-1).segs.find(s => s.cells > 0).hatch)`), false);
  });
}

test('node detail distinguishes lower-bound pod totals from independent node usage and idle percentages', () => {
  const data = adapt(snapshot([mc('mc-a', [node('node-a', [
    pod('partial', undefined, { coverage: { cpu: 0.5, memory: 1 } }),
    pod('missing', undefined, { usage: {} }),
  ])]) ]));
  const { run, elements } = render(data);
  run(`const n = allNodes(curMCs()[0].packing)[0]; renderNodePanel(document.getElementById('side'), n);`);
  const html = elements.side.innerHTML;
  assert.equal((html.match(/class="resource-card"/g) || []).length, 4);
  assert.match(html, /resource-secondary"><span>Pod usage<\/span> 0.01 vCPU <button[^>]*aria-label="cpu: 2 incomplete records"/);
  assert.match(html, /resource-secondary"><span>Pod usage<\/span> 0.02 GiB <button[^>]*aria-label="mem: 1 incomplete records"/);
  assert.match(html, /resource-value">1 vCPU<\/div><div class="resource-label">Measured node usage/);
  assert.match(html, /resource-value">1.95 GiB<\/div><div class="resource-label">Measured node usage/);
  assert.match(html, /width:6.25%/);
  assert.match(html, /Unused complement: 15 vCPU \(93.8%\)/);
  assert.match(html, /Unused complement: 62.05 GiB \(96.9%\)/);
  assert.match(html, /not scheduling headroom/);
  assert.doesNotMatch(html, /<details[^>]*open/);
  assert.match(html, /technical-details[\s\S]*node-a/);
  assert.equal(run('n.used.cpu_mc'), 10);
  run(`n.observed.node_usage = {}; renderNodePanel(document.getElementById('side'), n);`);
  assert.match(elements.side.innerHTML, /resource-value">Unknown<\/div><div class="resource-label">Measured node usage/);
  assert.doesNotMatch(elements.side.innerHTML, /Unused complement:/);
  assert.match(elements.side.innerHTML, /resource-bar unavailable/);
  run(`S.policy = 'requests'; renderNodePanel(document.getElementById('side'), allNodes(curMCs()[0].packing)[0]);`);
  assert.match(elements.side.innerHTML, /resource-secondary"><span>Pod requests<\/span> 0.2 vCPU/);
  assert.match(elements.side.innerHTML, /resource-value">1 vCPU<\/div><div class="resource-label">Measured node usage/);
});

test('pod tooltips and tables classify provenance and label partial numeric usage as lower bounds', () => {
  const data = adapt(snapshot([mc('mc-a', [node('node-a', [
    pod('pending', undefined, { phase: 'pending', usage: {}, usage_issues: ['pending'] }),
    pod('history', undefined, { current: false, phase: 'succeeded', coverage: { cpu: 0.5, memory: 1 }, usage_issues: ['sampling-gap'] }),
    pod('conflict', undefined, { usage: {}, usage_issues: ['lifecycle-conflict'] }),
    pod('no-phase', undefined, { usage: {}, usage_issues: ['pending'] }),
  ])]) ]));
  const { run, elements } = render(data);
  run(`const n = allNodes(curMCs()[0].packing)[0];`);
  const table = run('observedPodTable(n.pods)');
  assert.match(table, /Pending \(current at T\); unavailable \/ incomplete usage/);
  assert.match(table, /historical; last-known phase: succeeded; sampling gaps; unavailable \/ incomplete usage/);
  assert.match(table, /current at T; lifecycle conflict; unavailable \/ incomplete usage/);
  assert.match(table, /0.01 vCPU <button[^>]*class="coverage-badge"/);
  assert.doesNotMatch(table, /0.02 GiB <button/);
  assert.match(table, /Unknown <button[^>]*class="coverage-badge"/);
  assert.doesNotMatch(run('podUsageStatus(n.pods[3])'), /Pending/);
  for (const p of pods(data.usage.management_clusters[0])) {
    run(`tiles = []; drawHcpCard({ hcp: ${p.hcp}, pods: [n.pods.find(p => p.id === ${p.id})] }, 0, 0, 1);
      onMove({ clientX: 0.01, clientY: 14 });`);
    assert.ok(elements.tooltip.innerHTML.includes(run(`escapeHTML(podUsageStatus(n.pods.find(p => p.id === ${p.id})))`)));
    assert.doesNotMatch(elements.tooltip.innerHTML, /ocm-arohcp|node-a|namespace|UID/);
  }
  run(`S.policy = 'requests';`);
  assert.doesNotMatch(run('observedPodTable(allPods(curMCs()[0].packing))'), /class="coverage-badge"|historical/);
});

test('missing capacity retains [?] labels and ? donuts independently of pod completeness', () => {
  const data = adapt(snapshot([mc('mc-a', [node('node-a', [pod('complete')], {
    capacity: { cpu_mc: null, mem_mib: 65536, pods: 225, nic: null },
  })]) ]));
  const { run, labels, elements } = render(data);
  run(`const n = allNodes(curMCs()[0].packing)[0]; drawNodeTile(n, 0, 0, tileDims(n), 0); drawLegend();`);
  assert.equal(labels[0], '[?] node-a');
  assert.match(run(`donutSVG(n, 'cpu')`), />\?<\/text>/);
  assert.equal(run('n.incomplete.cpu_mc'), 0);
  assert.match(elements.legend.innerHTML, /\[\?\]: missing node capacity/);
  assert.match(elements.legend.innerHTML, /lower bound: incomplete pod totals/);
  assert.match(elements.legend.innerHTML, /independent node usage is in node detail, not added to pods/);
});

test('zero NIC is N/A, while missing capacity and missing measurements stay unknown', () => {
  const data = adapt(snapshot([mc('mc-a', [node('node-a', [pod('api')], {
    capacity: { cpu_mc: 16000, mem_mib: 65536, pods: 225, nic: 0 },
  })])]));
  const { run, elements } = render(data);
  run(`const n = allNodes(curMCs()[0].packing)[0]; renderNodePanel(document.getElementById('side'), n);`);
  assert.match(elements.side.innerHTML, /resource-value">N\/A/);
  assert.match(run(`donutSVG(n, 'nic')`), />N\/A<\/text>/);
  run(`n.full.nic = null; renderNodePanel(document.getElementById('side'), n);`);
  assert.match(elements.side.innerHTML, /Capacity unknown/);
  assert.doesNotMatch(elements.side.innerHTML, /resource-value">N\/A/);
});

test('HCP summaries group components but preserve escaped pod identity, provenance and navigation', () => {
  const unsafe = '<img src=x onerror="alert(1)">';
  const data = adapt(snapshot([mc('mc-a', [node('node-a', [
    pod('api-0', undefined, { component: 'api', uid: unsafe }),
    pod('api-1', undefined, { component: 'api', current: false }),
    pod(unsafe, undefined, { component: unsafe }),
  ])], { unplaced_pods: [pod('api-2', undefined, { component: 'api' })] })]));
  const { run, elements } = render(data);
  run(`S.selHcp = 1; updateSide();`);
  const html = elements.side.innerHTML;
  assert.match(html, /<h3>HCP 1 <span class="tag">Unknown<\/span>/);
  assert.equal((html.match(/class="resource-card"/g) || []).length, 4);
  assert.equal((html.match(/class="workload-group"/g) || []).length, 2);
  assert.match(html, /component-name" title="api">api<\/span><span>3<\/span><span>0.03<\/span>/);
  assert.match(html, /<span>vCPU<\/span><span>GiB<\/span>/);
  assert.equal((html.match(/class="pod-record"/g) || []).length, 4);
  assert.equal((html.match(/data-node="1"/g) || []).length, 3);
  assert.match(html, /Unplaced/);
  assert.match(html, /resource-value">3 pods<\/div>/);
  assert.match(html, /&lt;img src=x onerror=&quot;alert\(1\)&quot;&gt;/);
  assert.doesNotMatch(html, /<img|<details[^>]*open/);
  assert.match(run(`helpButton('<img src=x>', '"help')`), /data-tip="&amp;lt;img src=x&amp;gt;"/);
  run('S.selNode = allNodes(curMCs()[0].packing)[0]; renderNodePanel(document.getElementById("side"), S.selNode);');
  assert.doesNotMatch(elements.side.innerHTML, /class="node-link"/);
});

test('peak snapshots disclose metric, window, horizon and fleet score', () => {
  const output = {};
  const data = adapt(snapshot([], { at: '2026-09-01T12:15:00Z', start: '2026-09-01T12:00:00Z', window_seconds: 900, peak_selection: {
    metric: 'memory', score: 12 * 2 ** 30, search_start: 0, search_end: 604800, clusters: ['prod/mc'],
  } }), output);
  assert.match(output['obs-provenance'].notice.textContent, /15-minute average fleet memory over 7 days/);
  assert.match(output['obs-provenance'].notice.textContent, /12.00 GiB across 1 MCs/);
  assert.match(output['obs-summary'].textContent, /^T: 2026-09-01T12:15:00Z \/ window: 2026-09-01T12:00:00Z to T/);
  assert.equal(data.regional, false);
  assert.equal(render(data).run('laneHeaderHeight()'), 40);
});

test('regional null-top snapshots disclose original scores and each MC chosen window in both lenses', () => {
  const windows = [
    { environment: 'prod', region: 'eastus', at: '2026-09-01T12:45:00Z', start: '2026-09-01T12:30:00Z',
      original: '2026-09-01T12:15:00Z', shift: 1800 },
    { environment: 'prod', region: 'uksouth', at: '2026-09-03T00:05:00Z', start: '2026-09-02T23:50:00Z',
      original: '2026-09-03T00:05:00Z', shift: 0 },
  ].map(({ original, shift, ...entry }) => ({ ...entry, status: 'success', peak_selection: {
    environment: entry.environment, region: entry.region,
    original_selected_at: Date.parse(original) / 1000, selected_at: Date.parse(original) / 1000,
    original_start: Date.parse(original) / 1000 - 900,
    adjusted_at: Date.parse(entry.at) / 1000, adjusted_start: Date.parse(entry.start) / 1000,
    adjustment_seconds: shift, settle_seconds: 300,
    metric: 'memory', score: 12 * 2 ** 30, clusters: [`prod/${entry.region}-mc`],
  } }));
  const view = snapshot(windows.map((entry, i) => mc(`mc-${i}`, [node(`node-${i}`, [pod('api')])], entry)), {
    at: null, start: null, window_seconds: 900, peak_selection: null, regional_windows: windows,
  });
  const output = {}, data = adapt(view, output);
  assert.equal(data.regional, true);
  assert.match(output['obs-summary'].textContent, /^Regional peak snapshots — different times; not simultaneous fleet load/);
  assert.doesNotMatch(output['obs-summary'].textContent, /Unknown|T:/);
  const details = output['obs-provenance'];
  assert.equal(details.open, false);
  assert.equal(details.notice, undefined, 'no global fleet score notice');
  assert.match(details.children[0].textContent, /original peak windows, not the chosen shifted windows; shifted windows are not re-scored/);
  const table = details.children[1].children[0];
  assert.equal(table.tagName, 'table');
  assert.equal(table.children[1].children[0].children[3].textContent, 'Original peak score');
  const rows = table.children[2].children;
  assert.deepEqual(rows[0].children.map(cell => cell.textContent), [
    'prod / eastus', '2026-09-01T12:00:00.000Z to 2026-09-01T12:15:00.000Z',
    '2026-09-01T12:30:00Z to 2026-09-01T12:45:00Z', '12.00 GiB (memory)',
    'Shifted forward 1800s for HCP size settling (300s).',
  ]);
  assert.equal(rows[1].children[4].textContent, 'Unchanged (no forward shift needed).');
  for (const basis of ['usage', 'requests']) {
    const { run, labels, textDraws } = render(data, basis);
    assert.equal(run('laneHeaderHeight()'), 56);
    for (const [i, entry] of windows.entries()) {
      assert.equal(data[basis].management_clusters[i].observed.at, entry.at);
      assert.equal(data[basis].management_clusters[i].observed.start, entry.start);
      run(`{
        const lane = { mc: curMCs()[${i}], gi: ${i} };
        drawLaneNode(lane, 0, 360, 340, 1);
        drawLaneHcp(laneHcpData(lane, S.resource), 0, 360, 1, 100, 50, 2, 96, 8, S.resource);
      }`);
    }
    for (const label of ['2026-09-01 12:30–12:45 UTC', '2026-09-02 23:50–2026-09-03 00:05 UTC']) {
      assert.equal(labels.filter(text => text === label).length, 2, 'chosen UTC window appears in both lenses');
      assert.ok(textDraws.filter(draw => draw.text === label).every(draw => draw.x === 10 && draw.y === 48),
        'window has its own third header line, not appended to metadata or mix');
    }
    assert.ok(!labels.some(label => /12:00|12:15|Unknown.*T/.test(label)), 'lanes show collected times, not original peak times');
    assert.ok(run('tiles.every(tile => tile.ty >= 56)'), 'tiles begin below the regional header');
  }
  view.peak_selection = { scope: 'regional', regions: windows };
  adapt(view, output);
  assert.equal(output['obs-provenance'].notice, undefined, 'regional selection must never enter the global score renderer');
});

test('regional provenance safely displays blocked regions and never invents collected windows', () => {
  const unsafe = '<img src=x onerror="alert(1)">';
  const output = {};
  adapt(snapshot([], { at: null, start: null, window_seconds: 900, regional_windows: [
    { environment: 'prod', region: unsafe, status: 'blocked', reason: unsafe, peak_selection: {
      original_selected_at: 900, selected_at: 900, metric: 'cpu', score: 3.25,
    } },
  ] }), output);
  const row = output['obs-provenance'].children[1].children[0].children[2].children[0];
  assert.deepEqual(row.children.map(cell => cell.textContent), [
    `prod / ${unsafe}`, '1970-01-01T00:00:00.000Z to 1970-01-01T00:15:00.000Z',
    'Not collected', '3.25 cores (cpu)', unsafe,
  ]);
});

test('individual regional snapshots do not mislabel original peak scores as collected fleet scores', () => {
  const output = {};
  adapt(snapshot([], { at: '2026-09-01T12:45:00Z', start: '2026-09-01T12:30:00Z', window_seconds: 900,
    peak_selection: { environment: 'prod', region: 'eastus', metric: 'cpu', score: 3,
      original_selected_at: Date.parse('2026-09-01T12:15:00Z') / 1000, adjustment_seconds: 1800, settle_seconds: 300 },
  }), output);
  assert.equal(output['obs-provenance'].notice, undefined);
  assert.match(output['obs-provenance'].children[0].textContent, /not the chosen shifted windows/);
});

test('Brazil layout keeps the worker triplet together despite the missing infra pool', () => {
  const pools = ['infra1', 'infra2', 'system', 'userswft1', 'userswft2', 'userswft3'];
  const data = adapt(snapshot([mc('brazil', pools.map(pool => node(pool, [], { pool })))]));
  const { run } = render(data);
  assert.equal(run('JSON.stringify(laneNodeRows(curMCs()[0].packing, 4).map(row => row.map(g => g.nodes[0].observed.pool)))'),
    JSON.stringify([pools.slice(0, 3), pools.slice(3)]));
  assert.equal(run('JSON.stringify(laneNodeRows(curMCs()[0].packing, 2).map(row => row.length))'), '[2,1,2,1]');
  run('S.resource = "mem"; const lane = {mc:curMCs()[0], gi:0};');
  assert.equal(run('drawLaneNode(lane, 0, 1200, 280, 4)'), run('measureLaneNode(lane.mc.packing, 280, 4) + LANE_GAP'));
});

test('simulator keeps rounded pod cells, free remainder, reserve hatching and original legend', () => {
  const n = { sku: 'test', full: { cpu_mc: 2000 }, used: { cpu_mc: 200 },
    infra: { system: { cpu_mc: 500 }, daemonset: {}, buffer: {} },
    pods: [{ hcp: 1, cpu_mc: 100 }, { hcp: 1, cpu_mc: 100, reserve: 'rollout' }] };
  const { run, elements } = render({ minimal: { management_clusters: [{ packing: { zonal: [[n]], overflow: [] } }] } }, 'minimal');
  run(`const n = allNodes(curMCs()[0].packing)[0]; drawNodeTile(n, 0, 0, tileDims(n), 0); drawLegend();`);
  assert.equal(run('podCells(n.pods[0], "cpu")'), 1);
  assert.equal(run('tiles[0].segMap.at(-1).seg.kind'), 'free');
  assert.equal(run('tiles[0].segMap.at(-1).seg.hatch'), false);
  assert.equal(run('tiles[0].segMap.find(s => s.seg.pod?.reserve).seg.hatch'), true);
  assert.match(elements.legend.innerHTML, /system-reserved/);
  assert.doesNotMatch(elements.legend.innerHTML, /lower bound|outside known pod usage/);
  assert.match(run('donutSVG(n, "cpu")'), /HCP pods/);
  run(`renderNodePanel(document.getElementById('side'), n);`);
  assert.match(elements.side.innerHTML, /<h3>Node 1<\/h3>/);
  assert.match(elements.side.innerHTML, /resource-value">0.7 vCPU/);
  assert.match(elements.side.innerHTML, /rollout reserve/);
  assert.match(elements.side.innerHTML, /data-node="1"/);
  run(`S.selHcp = 1; updateSide();`);
  assert.match(elements.side.innerHTML, /resource-value">0.1 vCPU/);
  assert.match(elements.side.innerHTML, /1 reserve copies/);
});
