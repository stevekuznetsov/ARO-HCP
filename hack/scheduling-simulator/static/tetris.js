/* HCP scheduling simulator — tetris packing visualization (canvas + picking). */
'use strict';

const CELL_UNIT = { mem: 1024, cpu: 500, nic: 1, pods: 1 }; // absolute units per cell
const RES_LABEL = { mem: 'MiB', cpu: 'mC', nic: 'NIC', pods: 'pods' };
const TILE_COLS = 12;
const GAP = 1;
const COL_INFRA_SYS = '#232a37';
const COL_INFRA_DS = '#39435a';
const COL_INFRA_BUF = '#2e5a6b';
const COL_FREE = '#0c111b';
const COL_FREE_CELL = '#1b2434';   // visible "unused" cell
const COL_FREE_EDGE = '#0f1622';
const COL_UNKNOWN = '#70634b';
const RESERVE_COL = { rollout: '#9d6bf0', azdeath: '#ef6b4d', slot: '#d4a017' };

let S = { policy: 'minimal', resource: 'mem', lens: 'node', color: 'hcp',
          mc: 0, cellPx: 7, selHcp: null, selMC: 0, selNode: null, legendFocus: null };
let DATA = null, CFG = null, tiles = [], canvas = null, ctx = null;
let LANES = [];          // one entry per management cluster instance (swimlane)
let _activeLane = null;  // lane currently being drawn (tags tiles for click context)

// HCP whose cells are currently emphasised (pinned via click).
function activeHcp() { return S.selHcp; }
function segFocusDim(s) {
  // When a legend swatch is selected, dim every cell that doesn't match it.
  const f = S.legendFocus;
  if (!f) return false;
  if (f.cat === 'kind')    return s.kind !== f.val;               // system|daemonset|buffer
  if (f.cat === 'free')    return s.kind !== 'free' && !s.free;
  if (f.cat === 'reserve') return !(s.pod && s.pod.reserve === f.val);
  if (f.cat === 'size')    return !(s.pod && !s.pod.reserve && s.pod.hcp_size === f.val);
  if (f.cat === 'category' && f.val === 'non-hcp') return !s.pod || s.pod.category === 'HCP';
  if (f.cat === 'category' && f.val === 'historical') return !s.pod || (s.pod.observed.current === true && !podIncomplete(s.pod, S.resource));
  return false;
}

function initTetris() {
  DATA = JSON.parse(document.getElementById('sim-data').textContent);
  CFG = JSON.parse(document.getElementById('sim-cfg').textContent);
  if (!(S.policy in DATA)) S.policy = isObserved() ? 'usage' : 'minimal';
  canvas = document.getElementById('tetris');
  ctx = canvas.getContext('2d');

  applyHash();
  syncControlButtons();
  wireControls();
  canvas.addEventListener('mousemove', onMove);
  canvas.addEventListener('mouseleave', () => { hideTip(); });
  canvas.addEventListener('click', onClick);
  wireSideHelp();
  const side = document.getElementById('side');
  side.addEventListener('click', e => {
    const button = e.target.closest('[data-node]');
    if (!button) return;
    S.selNode = curMCs().flatMap(mc => allNodes(mc.packing)).find(n => panelNodeID(n) === Number(button.dataset.node));
    S.selHcp = null; render();
  });
  document.getElementById('legend').addEventListener('click', onLegendClick);
  if (!window.__tetrisWindowWired) {
    window.addEventListener('resize', () => { if (document.getElementById('tetris')) render(); });
    window.__tetrisWindowWired = true;
  }
  render();
}

/* optional state via URL hash, e.g. #lens=hcp&resource=nic&policy=legacy (also aids testing) */
function applyHash() {
  const h = (location.hash || '').replace(/^#/, '');
  if (!h) return;
  h.split('&').forEach(kv => {
    const [k, v] = kv.split('=');
    if (k === 'policy' && (isObserved() ? ['usage', 'requests'] : ['minimal', 'legacy']).includes(v)) S.policy = v;
    else if (k === 'resource' && CELL_UNIT[v]) S.resource = v;
    else if (k === 'lens' && (v === 'node' || v === 'hcp')) S.lens = v;
    else if (k === 'color' && (v === 'hcp' || v === 'component')) S.color = v;
    else if (k === 'mc') S.mc = +v || 0;
    else if (k === 'density') S.cellPx = +v || 7;
    else if (k === 'sel') S.selHcp = (v === '' ? null : +v);
    else if (k === 'node') S._nodePick = v;  // "zonal:az:idx" | "overflow:idx" (testing aid)
    else if (k === 'focus') {                 // "cat:val" e.g. reserve:slot, size:250, kind:buffer
      const [cat, val] = (v || '').split(':');
      S.legendFocus = cat ? { cat, val: (val != null ? val : null) } : null;
    }
  });
}
function syncControlButtons() {
  const map = { policy: S.policy, resource: S.resource, lens: S.lens, color: S.color, density: String(S.cellPx) };
  document.querySelectorAll('.seg').forEach(seg => {
    const g = seg.dataset.group; if (!(g in map)) return;
    seg.querySelectorAll('button').forEach(b => b.classList.toggle('on', b.dataset.val === map[g]));
  });
}

/* ---------- data helpers ---------- */
function isObserved() { return DATA?.mode === 'observed'; }
function allNodes(pk) { return pk.groups ? pk.groups.flatMap(g => g.nodes) : [...pk.zonal.flat(), ...pk.overflow]; }
function allPods(pk) { return [...allNodes(pk).flatMap(n => n.pods), ...(pk.unplaced || [])]; }
function escapeHTML(value) { return String(value ?? 'Unknown').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])); }
function sizeLabel(size) { return isObserved() ? String(size) : `${size}n`; }
function windowUsage(res) { return isObserved() && S.policy === 'usage' && (res === 'cpu' || res === 'mem'); }
function remainderKind(node, res) {
  if (windowUsage(res) && fullCap(node, res) != null) return 'outside-pods';
  return node.unknown?.[key(res)] ? 'unknown' : 'free';
}
function remainderLabel(kind) {
  if (kind === 'outside-pods') return 'capacity outside known pod usage (includes idle, host usage and any unmeasured pod usage)';
  return kind === 'free' ? (isObserved() ? 'unrequested capacity' : 'free / unused') : `${kind} / incomplete measurements`;
}
function remaining(node, res) {
  return Math.max(0, (fullCap(node, res) ?? 0) - infraCells3(node, res).reduce((a, b) => a + b.v, 0) - val(node.used, res));
}
function curMCs() { return DATA[S.policy].management_clusters || []; }
function hasPacking() {
  const mcs = curMCs();
  return mcs.length > 0 && mcs[0].packing;
}
function fullCap(node, res) {
  return node.full[key(res)];
}
function infraCells3(node, res) {
  const i = node.infra;
  return [{ kind: 'system', v: val(i.system, res), color: COL_INFRA_SYS },
          { kind: 'daemonset', v: val(i.daemonset, res), color: COL_INFRA_DS },
          { kind: 'buffer', v: val(i.buffer, res), color: COL_INFRA_BUF }];
}
function key(res) { return { mem: 'mem_mib', cpu: 'cpu_mc', nic: 'nic', pods: 'pods' }[res]; }
function val(obj, res) { return obj[key(res)] || 0; }
function podCells(pod, res) {
  if (isObserved()) return pod[key(res)] == null ? 1 : pod[key(res)] / CELL_UNIT[res];
  const v = res === 'pods' ? 1 : (pod[key(res)] || 0);
  return v > 0 ? Math.max(1, Math.ceil(v / CELL_UNIT[res])) : 0;
}
function cellsOf(amount, res) { return amount > 0 ? Math.ceil(amount / CELL_UNIT[res]) : 0; }

/* ---------- palettes ---------- */
// Each HCP *size* class gets its own two-point (light->dark) gradient (its own hue);
// within a size, HCPs are ordered along that gradient by id.
let hcpColorMap = {};   // hcp id -> css color
let sizeGrads = [];     // [{size, hue, c0, c1}] for the legend
let sizeHueMap = {};    // hcp_size -> hue (shared by tiles + mix line)
function sizeHue(i, n) { return Math.round((i * 360 / Math.max(n, 1) + 25) % 360); }
function sizeColor(hue, t) { const L = Math.round(70 - 30 * t); return `hsl(${hue},62%,${L}%)`; }
function computeHcpColors() {
  hcpColorMap = {}; sizeGrads = []; sizeHueMap = {};
  const mcs = curMCs();
  if (!mcs.length || !mcs[0].packing) return;
  const bySize = new Map();  // size -> Set(hcp ids), unioned across all MCs
  for (const mc of mcs) {
    if (!mc.packing) continue;
    for (const p of allPods(mc.packing)) {
      if (isObserved() && p.category !== 'HCP') continue;
      if (!bySize.has(p.hcp_size)) bySize.set(p.hcp_size, new Set());
      bySize.get(p.hcp_size).add(p.hcp);
    }
  }
  const sizes = [...bySize.keys()].sort((a, b) => isObserved() ? String(a).localeCompare(String(b)) : (+a) - (+b));
  sizes.forEach((sz, i) => {
    const hue = sizeHue(i, sizes.length);
    sizeHueMap[sz] = hue;
    const ids = [...bySize.get(sz)].sort((a, b) => a - b);
    const lo = ids[0], hi = ids[ids.length - 1];
    ids.forEach(id => { const t = hi > lo ? (id - lo) / (hi - lo) : 0.5; hcpColorMap[id] = sizeColor(hue, t); });
    sizeGrads.push({ size: sz, hue, c0: sizeColor(hue, 0), c1: sizeColor(hue, 1) });
  });
}

/* Per-MC HCP count by worker-node size, taken straight from the packing so it
   matches exactly what is drawn. Returns [{size, count, color}] big->small. */
function laneHcpMix(mc) {
  if (!mc || !mc.packing) return [];
  const bySize = new Map();
  for (const p of allPods(mc.packing)) {
    if (isObserved() && p.category !== 'HCP') continue;
    if (p.reserve) continue;
    let s = bySize.get(p.hcp_size); if (!s) { s = new Set(); bySize.set(p.hcp_size, s); }
    s.add(p.hcp);
  }
  return [...bySize.entries()]
    .map(([size, ids]) => ({ size, count: ids.size, color: sizeColor(sizeHueMap[size] ?? sizeHue(0, 1), 0.5) }))
    .sort((a, b) => isObserved() ? String(a.size).localeCompare(String(b.size)) : (+b.size) - (+a.size));
}

/* Reserved placeholder-slot HCPs held on this MC (count + representative size). */
function laneReserveInfo(mc) {
  if (!mc || !mc.packing) return null;
  const ids = new Set(); let size = null;
  for (const p of allPods(mc.packing)) {
    if (p.reserve === 'slot') { ids.add(p.hcp); size = p.hcp_size; }
  }
  return ids.size ? { count: ids.size, size } : null;
}

/* Draw "39×12n · 30×30n · …" starting at (x,y); each size label uses its tile colour.
   Returns the x coordinate just past the last segment. */
function drawMixLine(mix, x, y) {
  if (!mix.length) return x;
  ctx.font = '11px ui-sans-serif,system-ui';
  ctx.fillStyle = '#8b97a8'; ctx.fillText('mix:', x, y); x += ctx.measureText('mix:').width + 6;
  mix.forEach((m, i) => {
    const seg = `${m.count}×${sizeLabel(m.size)}`;
    ctx.fillStyle = m.color; ctx.fillText(seg, x, y); x += ctx.measureText(seg).width;
    if (i < mix.length - 1) { ctx.fillStyle = '#5b6675'; ctx.fillText('  ·  ', x, y); x += ctx.measureText('  ·  ').width; }
  });
  return x;
}
function hashHue(s) { let h = 0; for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) & 0xffff; return h % 360; }
function hcpColor(id) { return hcpColorMap[id] || '#888'; }
function compColor(name) { return `hsl(${hashHue(name)},50%,58%)`; }
function podColor(pod) {
  if (pod.reserve) return RESERVE_COL[pod.reserve] || '#888';
  if (isObserved() && S.color === 'hcp' && pod.category !== 'HCP') return compColor(pod.label);
  return S.color === 'hcp' ? hcpColor(pod.hcp) : compColor(pod.component);
}

/* ---------- controls ---------- */
function wireControls() {
  document.querySelectorAll('.seg').forEach(seg => {
    seg.addEventListener('click', e => {
      const b = e.target.closest('button'); if (!b) return;
      const group = seg.dataset.group, v = b.dataset.val;
      seg.querySelectorAll('button').forEach(x => x.classList.remove('on'));
      b.classList.add('on');
      if (group === 'policy') { S.policy = v; S.selHcp = null; S.selNode = null; S.selMC = 0; }
      else if (group === 'resource') S.resource = v;
      else if (group === 'lens') S.lens = v;
      else if (group === 'color') S.color = v;
      else if (group === 'density') S.cellPx = +v;
      render();
    });
  });
}

/* ---------- layout + draw ---------- */
function buildLanes() {
  // The model already de-duplicates identical MCs into `management_clusters`
  // entries with a `count` (e.g. full ×5, remainder ×1). One swimlane per shape.
  LANES = curMCs().map((mc, i) => ({ mc, gi: i }));
}

function render() {
  syncControlButtons();
  tiles = [];
  buildLanes();
  computeHcpColors();
  if (!hasPacking()) {
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    document.getElementById('side').textContent = 'No management clusters recorded.';
    drawLegend(); return;
  }
  if (S.lens === 'node') layoutNode();
  else layoutHcp();
  drawLegend();
  updateSide();
}

function tileDims(node) {
  const res = S.resource, cellPx = S.cellPx;
  const demandCells = isObserved() ? node.pods.reduce((sum, p) => sum + podCells(p, res), 0)
    + infraCells3(node, res).reduce((sum, b) => sum + b.v / CELL_UNIT[res], 0) : 0;
  const totalCells = Math.max(1, cellsOf(fullCap(node, res), res), Math.ceil(demandCells));
  const rows = Math.ceil(totalCells / TILE_COLS);
  const w = TILE_COLS * (cellPx + GAP) + 6;
  const h = rows * (cellPx + GAP) + 16;
  return { totalCells, rows, w, h };
}

function shortSku(name) { return name.replace('Standard_', '').replace(/s?_v\d+$/, ''); }
function skuSummary(nodes) {
  const m = new Map();
  for (const n of nodes) { const s = shortSku(n.sku); m.set(s, (m.get(s) || 0) + 1); }
  return [...m.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([s, c]) => `${c}×${s}`).join(' · ');
}
function orderNodes(nodes) {   // biggest first, so identical SKUs group together
  if (isObserved()) return [...nodes].sort((a, b) => (b.full.cpu_mc ?? 0) - (a.full.cpu_mc ?? 0) || (b.full.mem_mib ?? 0) - (a.full.mem_mib ?? 0));
  return [...nodes].sort((a, b) => (b.full.cpu - a.full.cpu) || (b.full.mem - a.full.mem));
}
function clipText(s, maxW) {   // ellipsize to fit a pixel width (font must be set already)
  if (ctx.measureText(s).width <= maxW) return s;
  let t = s;
  while (t.length > 1 && ctx.measureText(t + '…').width > maxW) t = t.slice(0, -1);
  return t + '…';
}

function sectionOfNodes(title, nodes, x0, y0, width, azIndex) {
  const headerH = 20;
  if (!nodes || !nodes.length) return y0;
  const ordered = orderNodes(nodes);
  const w = tileDims(ordered[0]).w;                       // width is constant (fixed TILE_COLS)
  const perRow = Math.max(1, Math.floor(width / w));
  ctx.fillStyle = '#9aa7b8'; ctx.font = '600 12px ui-sans-serif,system-ui';
  ctx.fillText(clipText(`${title} · ${ordered.length}n · ${skuSummary(ordered)}`, width - 4), x0, y0 + 14);
  let y = y0 + headerH;
  for (let i = 0; i < ordered.length; i += perRow) {
    const rowNodes = ordered.slice(i, i + perRow);
    let rowH = 0;
    rowNodes.forEach((node, j) => {
      const dims = tileDims(node);
      drawNodeTile(node, x0 + j * w, y, dims, azIndex);
      rowH = Math.max(rowH, dims.h);
    });
    y += rowH;
  }
  return y + 10;
}

function drawNodeTile(node, tx, ty, dims, azIndex) {
  const res = S.resource, cellPx = S.cellPx, step = cellPx + GAP;
  const gridCells = dims.totalCells;
  // background
  ctx.fillStyle = COL_FREE;
  ctx.fillRect(tx, ty + 14, TILE_COLS * step, dims.rows * step);
  if (isObserved()) {
    ctx.font = '10px ui-sans-serif,system-ui';
    ctx.fillStyle = node.unknown[key(res)] ? '#e0b45d' : '#8b97a8';
    ctx.fillText(clipText(`${fullCap(node, res) == null ? '[?] ' : ''}${node.incomplete[key(res)] ? '* ' : ''}${node.observed.current === true ? '' : 'retired: '}${node.observed.name}`, dims.w - 6), tx, ty + 11);
  }

  const segs = [];
  // infra: system, daemonset, buffer
  for (const b of infraCells3(node, res)) segs.push({ kind: b.kind, cells: isObserved() ? b.v / CELL_UNIT[res] : cellsOf(b.v, res), color: b.color });
  // pods: working first (sorted big->small), then reserves
  const work = node.pods.filter(p => !p.reserve).sort((a, b) => (b[key(res)] || 0) - (a[key(res)] || 0));
  const rsv = node.pods.filter(p => p.reserve);
  for (const p of work) { const c = podCells(p, res); if (c) segs.push({ pod: p, cells: c, color: podColor(p), hatch: isObserved() && (podIncomplete(p, res) || p.observed.current !== true) }); }
  for (const p of rsv) { const c = podCells(p, res); if (c) segs.push({ pod: p, cells: c, color: podColor(p), hatch: true }); }
  // free / unused capacity fills the remainder as visible cells
  const usedCells = segs.reduce((a, s) => a + s.cells, 0);
  const remainder = isObserved() ? remainderKind(node, res) : 'free';
  const remainderCells = isObserved() && fullCap(node, res) != null
    ? Math.max(0, fullCap(node, res) / CELL_UNIT[res] - usedCells) : Math.max(0, gridCells - usedCells);
  if (remainderCells > 0) segs.push({ kind: remainder, cells: remainderCells,
    color: remainder === 'unknown' ? COL_UNKNOWN : COL_FREE_CELL, free: remainder === 'free', hatch: remainder === 'unknown' });

  // paint cells in sequence, clamped to gridCells
  let ci = 0;
  const segMap = [];
  const act = activeHcp();
  for (const s of segs) {
    if (ci >= gridCells) break;
    const start = ci;
    const end = Math.min(gridCells, ci + s.cells);
    while (ci < end) {
      const cell = Math.floor(ci), fraction = ci - cell;
      const next = Math.min(end, cell + 1), width = (next - ci) * cellPx;
      const cx = tx + (cell % TILE_COLS) * step + fraction * cellPx;
      const cy = ty + 14 + Math.floor(cell / TILE_COLS) * step;
      const dim = (act != null && s.pod && s.pod.hcp !== act) || segFocusDim(s);
      ctx.fillStyle = dim ? dimColor(s.color) : s.color;
      ctx.fillRect(cx, cy, width, cellPx);
      if (s.free) { ctx.strokeStyle = COL_FREE_EDGE; ctx.strokeRect(cx + .5, cy + .5, cellPx - 1, cellPx - 1); }
      if (s.hatch && !dim) { ctx.strokeStyle = 'rgba(0,0,0,.5)'; ctx.beginPath(); ctx.moveTo(cx, cy + cellPx); ctx.lineTo(cx + cellPx, cy); ctx.stroke(); }
      if (act != null && s.pod && s.pod.hcp === act) { ctx.strokeStyle = '#fff'; ctx.strokeRect(cx + .5, cy + .5, cellPx - 1, cellPx - 1); }
      ci = next;
    }
    segMap.push({ start, end: ci, seg: s });
  }
  if (isObserved()) {
    const cap = fullCap(node, res);
    if (cap != null && val(node.used, res) + val(node.infra.system, res) > cap) {
      ctx.strokeStyle = '#ff5d5d'; ctx.strokeRect(tx, ty + 14, TILE_COLS * step, dims.rows * step);
    }
  }
  if (node === S.selNode) {   // highlight the node whose detail panel is open
    ctx.strokeStyle = '#4f9dff'; ctx.lineWidth = 2;
    ctx.strokeRect(tx - 1, ty + 13, TILE_COLS * step + 1, dims.rows * step + 1);
    ctx.lineWidth = 1;
  }
  tiles.push({ tx, ty: ty + 14, cols: TILE_COLS, step, cellPx, rows: dims.rows, segMap, node, azIndex, lane: _activeLane });
}

const LANE_HEADER = 40, LANE_GAP = 14, LANE_PAD = 8;

function laneHeaderHeight() { return isObserved() && DATA.regional ? 56 : LANE_HEADER; }

function drawRegionalWindow(mc, y0) {
  if (!isObserved() || !DATA.regional) return;
  const start = mc.observed.start ? new Date(mc.observed.start) : null;
  const end = mc.observed.at ? new Date(mc.observed.at) : null;
  let label = 'Observation window unavailable';
  if (start && end && Number.isFinite(+start) && Number.isFinite(+end)) {
    const s = start.toISOString(), e = end.toISOString();
    label = `${s.slice(0, 16).replace('T', ' ')}–${s.slice(0, 10) === e.slice(0, 10) ? e.slice(11, 16) : e.slice(0, 16).replace('T', ' ')} UTC`;
  }
  ctx.font = '11px ui-sans-serif,system-ui'; ctx.fillStyle = '#b7c4d6';
  ctx.fillText(label, 10, y0 + 48);
}

function laneColsNode(pk) {
  if (pk.groups) return pk.groups;
  return [
    { title: 'Zonal AZ1', nodes: pk.zonal[0] || [], az: 0 },
    { title: 'Zonal AZ2', nodes: pk.zonal[1] || [], az: 1 },
    { title: 'Zonal AZ3', nodes: pk.zonal[2] || [], az: 2 },
    { title: 'Overflow', nodes: pk.overflow || [], az: -1 },
  ];
}

function laneNodeRows(pk, columnCount) {
  const cols = laneColsNode(pk);
  const bands = pk.groups ? ['platform', 'workers', 'unknown'].map(band =>
    cols.filter(c => (c.layoutBand || 'unknown') === band)) : [cols];
  const rows = [];
  for (const band of bands) {
    for (let i = 0; i < band.length; i += columnCount) rows.push(band.slice(i, i + columnCount));
  }
  return rows;
}

function measureLaneNode(pk, secW, columnCount = 4) {
  let h = 0;
  for (const row of laneNodeRows(pk, columnCount)) h += Math.max(...row.map(c => measureSection(c.nodes, secW)));
  return laneHeaderHeight() + h + LANE_PAD;
}

function drawLaneNode(lane, y0, W, secW, columnCount = 4) {
  const pk = lane.mc.packing;
  const laneH = measureLaneNode(pk, secW, columnCount);
  // alternating swimlane background + left accent + separator
  ctx.fillStyle = (lane.gi % 2) ? 'rgba(255,255,255,.015)' : 'rgba(79,157,255,.03)';
  ctx.fillRect(0, y0, W + 2, laneH - 4);
  ctx.fillStyle = '#4f9dff'; ctx.fillRect(0, y0, 3, laneH - 4);
  // header
  const cnt = lane.mc.count || 1;
  const zc = pk.zonal?.reduce((a, az) => a + az.length, 0), oc = pk.overflow?.length;
  const t1 = isObserved() ? lane.mc.name : `${lane.mc.kind === 'full' ? 'Full' : 'Remainder'} MC${cnt > 1 ? ` ×${cnt}` : ''}`;
  ctx.font = '700 13px ui-sans-serif,system-ui'; ctx.fillStyle = '#e6ebf2';
  ctx.fillText(t1, 10, y0 + 16);
  const w = ctx.measureText(t1).width;
  ctx.font = '11px ui-sans-serif,system-ui'; ctx.fillStyle = '#8b97a8';
  ctx.fillText(isObserved() ? `  ·  ${lane.mc.observed.environment} / ${lane.mc.observed.region} · ${lane.mc.hcps} HCP · ${allNodes(pk).length} nodes · ${pk.unplaced.length} unplaced pods`
    : `  ·  ${lane.mc.hcps} HCP each  ·  ${zc} zonal + ${oc} overflow nodes${cnt > 1 ? `  ·  ${cnt} identical clusters` : ''}`, 10 + w + 6, y0 + 16);
  const mixEnd = drawMixLine(laneHcpMix(lane.mc), 10, y0 + 32);
  const rs = laneReserveInfo(lane.mc);
  if (rs) { ctx.fillStyle = RESERVE_COL.slot; ctx.font = '11px ui-sans-serif,system-ui'; ctx.fillText(`  ·  + ${rs.count} reserved ${rs.size}n`, mixEnd + 4, y0 + 32); }
  drawRegionalWindow(lane.mc, y0);
  // Platform and worker pools start on separate rows, even if a pool is absent.
  let y = y0 + laneHeaderHeight();
  for (const row of laneNodeRows(pk, columnCount)) {
    row.forEach((c, j) => sectionOfNodes(c.title, c.nodes, j * (secW + 8) + 10, y, secW, c.az));
    y += Math.max(...row.map(c => measureSection(c.nodes, secW)));
  }
  return y0 + laneH + LANE_GAP;
}

function layoutNode() {
  const wrap = document.getElementById('canvas-wrap');
  const W = wrap.clientWidth - 4;
  const columnCount = isObserved() ? Math.max(1, Math.min(4, Math.max(1, ...LANES.map(l => l.mc.packing.groups.length)), Math.floor((W - 10) / (TILE_COLS * (S.cellPx + GAP) + 24)))) : 4;
  const secW = Math.floor((W - 10) / columnCount) - 8;
  ctx.clearRect(0, 0, canvas.width, canvas.height);
  // measure total height
  let total = 6;
  for (const lane of LANES) total += measureLaneNode(lane.mc.packing, secW, columnCount) + LANE_GAP;
  setCanvasSize(W, total);
  let y = 6;
  for (const lane of LANES) {
    _activeLane = lane;
    y = drawLaneNode(lane, y, W, secW, columnCount);
  }
  _activeLane = null;
}

function measureSection(nodes, width) {
  if (!nodes || !nodes.length) return 30;
  const ordered = orderNodes(nodes);
  const w = tileDims(ordered[0]).w;
  const perRow = Math.max(1, Math.floor(width / w));
  let h = 20;
  for (let i = 0; i < ordered.length; i += perRow) {
    const rowNodes = ordered.slice(i, i + perRow);
    h += Math.max(...rowNodes.map(n => tileDims(n).h));
  }
  return h + 10;
}

/* ---------- HCP lens ---------- */
function laneHcpData(lane, res) {
  const pk = lane.mc.packing;
  const byHcp = new Map();
  let infraSys = 0, infraDs = 0, infraBuf = 0, free = 0, unknown = 0, outsidePods = 0;
  for (const n of allNodes(pk)) {
    infraSys += val(n.infra.system, res); infraDs += val(n.infra.daemonset, res); infraBuf += val(n.infra.buffer, res);
    if (isObserved() && remainderKind(n, res) === 'unknown') unknown += remaining(n, res);
    else if (isObserved() && remainderKind(n, res) === 'outside-pods') outsidePods += remaining(n, res);
    else free += Math.max(0, fullCap(n, res) - val(n.infra.system, res) - val(n.infra.daemonset, res)
      - val(n.infra.buffer, res) - n.pods.reduce((a, p) => a + (p[key(res)] || 0), 0));
  }
  for (const p of allPods(pk)) { const g = byHcp.get(p.hcp) || []; g.push(p); byHcp.set(p.hcp, g); }
  return { lane, byHcp, infraSys, infraDs, infraBuf, free, unknown, outsidePods };
}

function measureLaneHcp(ld, perRow, cardH, bandCols, step, res) {
  const cardRows = Math.ceil(ld.byHcp.size / perRow);
  const infraCells = cellsOf(ld.infraSys, res) + cellsOf(ld.infraDs, res) + cellsOf(ld.infraBuf, res);
  const bandRows = Math.ceil((infraCells || 1) / bandCols) + Math.ceil((cellsOf(ld.free, res) + cellsOf(ld.unknown, res) + cellsOf(ld.outsidePods, res) || 1) / bandCols);
  return laneHeaderHeight() + cardRows * cardH + 8 + bandRows * step + 60;
}

function layoutHcp() {
  const W = document.getElementById('canvas-wrap').clientWidth - 4;
  const res = S.resource;
  let maxCells = 1;
  const laneData = LANES.map(lane => {
    const ld = laneHcpData(lane, res);
    for (const g of ld.byHcp.values()) { const c = g.reduce((a, p) => a + podCells(p, res), 0); if (c > maxCells) maxCells = c; }
    return ld;
  });
  const rows = Math.ceil(maxCells / TILE_COLS);
  const cardW = TILE_COLS * (S.cellPx + GAP) + 8;
  const cardH = rows * (S.cellPx + GAP) + 22;
  const step = S.cellPx + GAP, bandCols = Math.max(1, Math.floor((W - 12) / step));
  const perRow = Math.max(1, Math.floor((W - 12) / cardW));
  let total = 6;
  for (const ld of laneData) total += measureLaneHcp(ld, perRow, cardH, bandCols, step, res) + LANE_GAP;
  ctx.clearRect(0, 0, canvas.width, canvas.height);
  setCanvasSize(W, total);
  let y = 6;
  for (const ld of laneData) {
    _activeLane = ld.lane;
    y = drawLaneHcp(ld, y, W, perRow, cardW, cardH, rows, bandCols, step, res);
  }
  _activeLane = null;
}

function drawLaneHcp(ld, y0, W, perRow, cardW, cardH, rows, bandCols, step, res) {
  const lane = ld.lane;
  const h = measureLaneHcp(ld, perRow, cardH, bandCols, step, res);
  ctx.fillStyle = (lane.gi % 2) ? 'rgba(255,255,255,.015)' : 'rgba(79,157,255,.03)';
  ctx.fillRect(0, y0, W + 2, h - 4);
  ctx.fillStyle = '#4f9dff'; ctx.fillRect(0, y0, 3, h - 4);
  const cnt = lane.mc.count || 1;
  const t1 = isObserved() ? lane.mc.name : `${lane.mc.kind === 'full' ? 'Full' : 'Remainder'} MC${cnt > 1 ? ` ×${cnt}` : ''}`;
  ctx.font = '700 13px ui-sans-serif,system-ui'; ctx.fillStyle = '#e6ebf2';
  ctx.fillText(t1, 10, y0 + 16);
  const w = ctx.measureText(t1).width;
  ctx.font = '11px ui-sans-serif,system-ui'; ctx.fillStyle = '#8b97a8';
  ctx.fillText(isObserved() ? `  ·  ${lane.mc.hcps} HCP · namespaces kept individually · includes unplaced workloads`
    : `  ·  ${lane.mc.hcps} HCP each${cnt > 1 ? `  ·  ${cnt} identical clusters` : ''}`, 10 + w + 6, y0 + 16);
  const mixEnd2 = drawMixLine(laneHcpMix(lane.mc), 10, y0 + 32);
  const rs2 = laneReserveInfo(lane.mc);
  if (rs2) { ctx.fillStyle = RESERVE_COL.slot; ctx.font = '11px ui-sans-serif,system-ui'; ctx.fillText(`  ·  + ${rs2.count} reserved ${rs2.size}n`, mixEnd2 + 4, y0 + 32); }
  drawRegionalWindow(lane.mc, y0);
  const hcpIds = [...ld.byHcp.keys()].sort((a, b) => a - b);
  hcpIds.forEach((id, idx) => {
    const cx = (idx % perRow) * cardW + 10;
    const cy = y0 + laneHeaderHeight() + Math.floor(idx / perRow) * cardH;
    drawHcpCard({ hcp: id, pods: ld.byHcp.get(id) }, cx, cy, rows);
  });
  let by = y0 + laneHeaderHeight() + Math.ceil(hcpIds.length / perRow) * cardH + 6;
  by = drawBand(isObserved() ? 'current capacity minus allocatable (requests / counts only)' : 'infrastructure (system-reserved + daemonset + buffer)',
    [{ kind: 'system', cells: cellsOf(ld.infraSys, res), color: COL_INFRA_SYS },
     { kind: 'daemonset', cells: cellsOf(ld.infraDs, res), color: COL_INFRA_DS },
     { kind: 'buffer', cells: cellsOf(ld.infraBuf, res), color: COL_INFRA_BUF }], 10, by, W, bandCols);
  drawBand(isObserved() ? (windowUsage(res) ? 'outside known pod usage (dark; not necessarily idle)' : 'unrequested (dark); unavailable capacity / requests (hatched)') : 'free / unused',
    [{ kind: 'free', cells: cellsOf(ld.free, res), color: COL_FREE_CELL },
     { kind: 'outside-pods', cells: cellsOf(ld.outsidePods, res), color: COL_FREE_CELL },
     { kind: 'unknown', cells: cellsOf(ld.unknown, res), color: COL_UNKNOWN, hatch: true }], 10, by, W, bandCols);
  return y0 + h + LANE_GAP;
}

function drawBand(label, segs, x, y, width, cols) {
  const cellPx = S.cellPx, step = cellPx + GAP;
  ctx.fillStyle = '#9aa7b8'; ctx.font = '600 12px ui-sans-serif,system-ui';
  ctx.fillText(label, x, y + 12);
  const y0 = y + 18;
  let ci = 0;
  for (const s of segs) { const dim = segFocusDim(s); for (let k = 0; k < s.cells; k++) {
    const cx = x + (ci % cols) * step, cy = y0 + Math.floor(ci / cols) * step;
    ctx.fillStyle = dim ? dimColor(s.color) : s.color; ctx.fillRect(cx, cy, cellPx, cellPx); ci++;
    if (s.hatch && !dim) { ctx.strokeStyle = 'rgba(0,0,0,.5)'; ctx.beginPath(); ctx.moveTo(cx, cy + cellPx); ctx.lineTo(cx + cellPx, cy); ctx.stroke(); }
  } }
  const rows = Math.ceil((ci || 1) / cols);
  return y0 + rows * step + 12;
}

function drawHcpCard(card, x, y, rows) {
  const res = S.resource, cellPx = S.cellPx, step = cellPx + GAP;
  const act = activeHcp();
  const isAct = (card.hcp != null && card.hcp === act);
  ctx.fillStyle = COL_FREE; ctx.fillRect(x, y + 14, TILE_COLS * step, rows * step);
  ctx.font = '10px ui-sans-serif,system-ui';
  const lbl = card.synth ? card.label : isObserved() ? `${card.pods[0].label} · ${sizeLabel(card.pods[0].hcp_size)}` : `HCP ${card.hcp} · ${card.pods[0].hcp_size}n`;
  ctx.fillStyle = isAct ? '#fff' : '#66738a';
  ctx.fillText(isObserved() ? clipText(lbl, TILE_COLS * step) : lbl, x, y + 11);

  const segs = [];
  if (card.synth === 'infra') {
    segs.push({ kind: 'system', cells: cellsOf(card.sys, res), color: COL_INFRA_SYS });
    segs.push({ kind: 'daemonset', cells: cellsOf(card.ds, res), color: COL_INFRA_DS });
  } else if (card.synth === 'free') {
    segs.push({ kind: 'free', cells: cellsOf(card.amt, res), color: '#182031' });
  } else {
    const work = card.pods.filter(p => !p.reserve).sort((a, b) => (b[key(res)] || 0) - (a[key(res)] || 0));
    const rsv = card.pods.filter(p => p.reserve);
    for (const p of work) { const c = podCells(p, res); if (c) segs.push({ pod: p, cells: c, color: podColor(p), hatch: isObserved() && (podIncomplete(p, res) || p.observed.current !== true) }); }
    for (const p of rsv) { const c = podCells(p, res); if (c) segs.push({ pod: p, cells: c, color: podColor(p), hatch: true }); }
  }
  let ci = 0; const segMap = [];
  const dimCard = (act != null && card.hcp != null && !isAct);
  for (const s of segs) {
    const start = ci;
    const dim = dimCard || segFocusDim(s);
    const end = ci + s.cells;
    while (ci < end) {
      const cell = Math.floor(ci), next = Math.min(end, cell + 1);
      const px = x + (cell % TILE_COLS) * step + (ci - cell) * cellPx;
      const py = y + 14 + Math.floor(cell / TILE_COLS) * step;
      ctx.fillStyle = dim ? dimColor(s.color) : s.color;
      ctx.fillRect(px, py, (next - ci) * cellPx, cellPx);
      if (s.hatch && !dim) { ctx.strokeStyle = 'rgba(0,0,0,.45)'; ctx.beginPath(); ctx.moveTo(px, py + cellPx); ctx.lineTo(px + cellPx, py); ctx.stroke(); }
      ci = next;
    }
    segMap.push({ start, end: ci, seg: s });
  }
  if (isAct) { ctx.strokeStyle = '#fff'; ctx.lineWidth = 1.5; ctx.strokeRect(x - 1, y + 13, TILE_COLS * step + 2, rows * step + 2); ctx.lineWidth = 1; }
  tiles.push({ tx: x, ty: y + 14, cols: TILE_COLS, step, cellPx, rows, segMap, node: null, card, lane: _activeLane });
}

/* ---------- canvas sizing (HiDPI) ---------- */
function setCanvasSize(cssW, cssH) {
  const dpr = window.devicePixelRatio || 1;
  const w = Math.floor(cssW * dpr), h = Math.floor(cssH * dpr);
  if (canvas.width !== w || canvas.height !== h) {
    canvas.style.width = cssW + 'px'; canvas.style.height = cssH + 'px';
    canvas.width = w; canvas.height = h;
  } else {
    ctx.clearRect(0, 0, w, h);
  }
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
}

/* ---------- picking ---------- */
function tileAt(mx, my) {
  for (const t of tiles) {
    const w = t.cols * t.step, h = Math.ceil((t.segMap.length ? t.segMap[t.segMap.length - 1].end : 0) / t.cols) * t.step;
    if (isObserved() && mx >= t.tx && mx < t.tx + w && my >= t.ty - 14 && my < t.ty + t.rows * t.step) {
      if (my < t.ty) return { t, seg: null };
      const col = Math.floor((mx - t.tx) / t.step), row = Math.floor((my - t.ty) / t.step);
      const cell = row * t.cols + col + Math.min(.999999, ((mx - t.tx) % t.step) / t.cellPx);
      return { t, seg: t.segMap.find(s => cell >= s.start && cell < s.end)?.seg || null };
    }
    if (mx >= t.tx && mx < t.tx + w && my >= t.ty && my < t.ty + h + t.step) {
      const col = Math.floor((mx - t.tx) / t.step), row = Math.floor((my - t.ty) / t.step);
      const cell = row * t.cols + col + (isObserved() ? Math.min(.999999, ((mx - t.tx) % t.step) / t.cellPx) : 0);
      for (const sm of t.segMap) if (cell >= sm.start && cell < sm.end) return { t, seg: sm.seg };
      return { t, seg: null };
    }
  }
  return null;
}
function onMove(e) {
  const r = canvas.getBoundingClientRect();
  const hit = tileAt(e.clientX - r.left, e.clientY - r.top);
  const seg = hit && hit.seg;
  if (!seg) {
    if (isObserved() && hit) {
      canvas.style.cursor = 'pointer';
      showTip(e.clientX, e.clientY, escapeHTML(hit.t.node?.observed.name ?? hit.t.card?.pods[0]?.label));
    } else { hideTip(); canvas.style.cursor = 'default'; }
    return;
  }
  canvas.style.cursor = (seg.pod || (hit.t.card && hit.t.card.hcp != null) || hit.t.node) ? 'pointer' : 'default';
  let html;
  if (seg.pod) {
    const p = seg.pod;
    html = `<b>${escapeHTML(p.component)}</b> · ${escapeHTML(isObserved() ? p.label : `HCP ${p.hcp}`)}<br>`
      + `${isObserved() ? S.policy : 'Requests'}: ${panelAmount(p.cpu_mc, 'cpu')} · ${panelAmount(p.mem_mib, 'mem')}`
      + (isObserved() ? `<br>${escapeHTML(podUsageStatus(p))}${['cpu', 'mem'].some(r => podIncomplete(p, r)) ? ' · lower bound' : ''}`
        : `<br>${escapeHTML(p.tier)}${p.reserve ? ` · ${escapeHTML(p.reserve)} reserve` : ''}`);
  } else if (seg.kind === 'free') {
    html = `<b>${remainderLabel('free')}</b>`;
  } else if (seg.kind) {
    html = `<b>${escapeHTML(isObserved() && seg.kind !== 'system' ? remainderLabel(seg.kind) : seg.kind)}</b>`;
  }
  if (isObserved() && hit.t.node && !seg.pod) {
    const n = hit.t.node, res = S.resource;
    html = (html || '') + `<br>${escapeHTML(n.observed.name)} · ${escapeHTML(n.sku)} · ${n.observed.current === true ? 'current' : 'retired'}<br>`
      + `Capacity: ${observedAmount(fullCap(n, res), res)} · known pod total: ${knownPodTotal(n, res)}<br>`
      + `${n.unknown[key(res)] ? 'Unknown / incomplete coverage. ' : ''}${remainderLabel(remainderKind(n, res))}`;
  }
  if (html) showTip(e.clientX, e.clientY, html); else hideTip();
}
function onClick(e) {
  const r = canvas.getBoundingClientRect();
  const hit = tileAt(e.clientX - r.left, e.clientY - r.top);
  if (!hit) return;
  const laneMC = hit.t.lane ? hit.t.lane.mc : curMCs()[0];
  if (isObserved() && hit.t.node && e.shiftKey) {
    S.selNode = hit.t.node; S.selHcp = null; render();
  } else if (hit.seg && hit.seg.pod) {
    const hcp = hit.seg.pod.hcp;
    S.selHcp = (S.selHcp === hcp) ? null : hcp;
    S.selMC = laneMC;
    S.selNode = null;
    render();
  } else if (hit.t.node) {
    S.selNode = (S.selNode === hit.t.node) ? null : hit.t.node;
    S.selHcp = null;
    render();
  } else if (isObserved() && hit.t.card) {
    S.selHcp = hit.t.card.hcp; S.selMC = laneMC; S.selNode = null; render();
  }
}

/* ---------- tooltip + side panel ---------- */
let tipAnchor = null, tipPinned = false, tipTimer;
function wireSideHelp() {
  if (window.__tetrisHelpWired) return;
  window.__tetrisHelpWired = true;
  const open = a => {
    clearTimeout(tipTimer);
    tipAnchor?.removeAttribute('aria-describedby');
    tipAnchor = a;
    a.setAttribute('aria-describedby', 'tooltip');
    const r = a.getBoundingClientRect();
    showTip(r.left, r.bottom, a.dataset.tip);
  };
  const later = () => {
    clearTimeout(tipTimer);
    tipTimer = setTimeout(() => {
      if (!tipPinned && document.activeElement !== tipAnchor) hideTip();
    }, 180);
  };
  document.addEventListener('pointerover', e => {
    const a = e.target.closest('#side [data-tip]');
    if (a && !tipPinned) open(a);
    if (e.target.closest('#tooltip')) clearTimeout(tipTimer);
  });
  document.addEventListener('pointerout', e => {
    if (e.target.closest('#side [data-tip], #tooltip')) later();
  });
  document.addEventListener('focusin', e => {
    const a = e.target.closest('#side [data-tip]');
    if (a) { tipPinned = false; open(a); }
  });
  document.addEventListener('focusout', e => {
    if (e.target.closest('#side [data-tip]')) { tipPinned = false; later(); }
  });
  document.addEventListener('click', e => {
    const a = e.target.closest('#side [data-tip]');
    if (a) {
      if (tipPinned && tipAnchor === a) { tipPinned = false; hideTip(); }
      else { tipPinned = false; open(a); tipPinned = true; }
    } else if (!e.target.closest('#tooltip')) { tipPinned = false; hideTip(); }
  });
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape') { tipPinned = false; hideTip(); }
  });
}
function showTip(x, y, html) {
  if (tipPinned) return;
  const t = document.getElementById('tooltip'); t.innerHTML = html; t.style.display = 'block';
  t.setAttribute?.('role', 'tooltip');
  t.style.left = Math.max(8, Math.min(x + 12, window.innerWidth - t.offsetWidth - 8)) + 'px';
  t.style.top = Math.max(8, Math.min(y + 12, window.innerHeight - t.offsetHeight - 8)) + 'px';
}
function hideTip() {
  if (tipPinned) return;
  clearTimeout(tipTimer);
  tipAnchor?.removeAttribute('aria-describedby'); tipAnchor = null;
  document.getElementById('tooltip').style.display = 'none';
}
function fmt(n) { return Math.round(n).toLocaleString(); }
function observedAmount(value, res) { return typeof value !== 'number' || !Number.isFinite(value) || value < 0 ? 'Unknown' : `${Number(value.toFixed(2)).toLocaleString()} ${RES_LABEL[res]}`; }
function coverageLabel(value) { return value == null ? 'Unknown' : `${Math.round(value * 100)}%`; }
function podIncomplete(p, res) {
  return p[key(res)] == null || (windowUsage(res) && !(p.observed.coverage?.[res === 'cpu' ? 'cpu' : 'memory'] >= 1));
}
function observedPodAmount(p, res) {
  return observedAmount(p[key(res)], res) + (p[key(res)] != null && podIncomplete(p, res) ? ' (lower bound)' : '');
}
function knownPodTotal(node, res) {
  const count = node.incomplete[key(res)];
  return observedAmount(val(node.used, res), res) + (count ? ` (lower bound; ${count} pods with missing/incomplete ${windowUsage(res) ? 'usage' : 'requests'})` : '');
}
function podUsageStatus(p) {
  const o = p.observed, issues = o.usage_issues || [];
  const incomplete = ['cpu', 'memory'].some(r => !(o.coverage?.[r] >= 1))
    || o.usage?.cpu_mc == null || o.usage?.mem_mib == null;
  const labels = [o.current === true ? (o.phase === 'pending' ? 'Pending (current at T)' : 'current at T') : 'historical'];
  if (o.current !== true && o.phase) labels.push(`last-known phase: ${o.phase}`);
  if (issues.includes('lifecycle-conflict')) labels.push('lifecycle conflict');
  if (issues.includes('sampling-gap')) labels.push('sampling gaps');
  if (incomplete) labels.push('unavailable / incomplete usage');
  return labels.join('; ');
}

function updateSide() {
  tipPinned = false; hideTip();
  const side = document.getElementById('side');
  if (S.selNode) { renderNodePanel(side, S.selNode); return; }
  if (S.selHcp == null) {
    side.innerHTML = '<div class="side-empty muted">click an HCP to isolate it · click a node for detail'
      + (isObserved() ? ' · shift-click any node cell for node detail<br>Usage includes historical pods; requests, NIC and pod counts are current at T. Unknown values use hatched markers, not zero. Red outline: known demand exceeds capacity.' : '') + '</div>';
    return;
  }
  // gather this HCP's pods within the management cluster it was selected in
  const mc = (S.selMC && S.selMC.packing) ? S.selMC : curMCs()[0]; const pk = mc.packing;
  const pods = allPods(pk).filter(p => p.hcp === S.selHcp);
  if (!pods.length) { side.innerHTML = ''; return; }
  const first = pods[0], work = pods.filter(p => !p.reserve);
  side.innerHTML = panelHead(isObserved() ? first.label : `HCP ${S.selHcp}`, `${work.length} records${pods.length > work.length ? ` + ${pods.length - work.length} reserve copies` : ''}`, sizeLabel(first.hcp_size))
    + `<div class="resource-grid">${['cpu', 'mem', 'nic', 'pods'].map(r => resourceCard(r, podTotal(work, r),
      isObserved() ? (r === 'nic' ? 'Requested at T' : r === 'pods' ? 'Current at T' : `Pod ${S.policy}`) : 'Requested',
      undefined, '', resourceHelp(r))).join('')}</div>`
    + technicalDetails(isObserved() ? { cluster: mc.name, namespace: first.observed.namespace, hcp: first.observed.hcp ?? first.observed.hcp_id } : { size: first.hcp_size, reserveCopies: pods.length - work.length })
    + workloadTable(pods);
}
function clearSel() { S.selHcp = null; S.selNode = null; render(); }

function panelAmount(value, res) {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0) return 'Unknown';
  const amount = value / (res === 'cpu' ? 1000 : res === 'mem' ? 1024 : 1);
  const unit = { cpu: 'vCPU', mem: 'GiB', nic: 'NIC', pods: 'pods' }[res];
  return `${amount > 0 && amount < .01 ? '<0.01' : Number(amount.toFixed(2)).toLocaleString()} ${unit}`.replace('<', '&lt;');
}
function panelNodeID(node) {
  return isObserved() ? node.id : curMCs().flatMap(mc => allNodes(mc.packing)).indexOf(node) + 1;
}
function helpButton(text, label = 'Help', badge = false) {
  return `<button type="button" class="${badge ? 'coverage-badge' : 'help-button'}" aria-label="${escapeHTML(label)}" data-tip="${escapeHTML(escapeHTML(text))}">${badge ? 'lower bound' : '?'}</button>`;
}
function resourceHelp(res) {
  if (!isObserved()) return 'Simulated requests, not measured usage. Node allocation includes infrastructure and reserve copies; HCP summaries exclude reserve copies.';
  if (res === 'nic') return 'SWIFT NIC requests at T are reservations, not measured attachments. Allocatable is not free slots. Zero capacity means N/A.';
  if (res === 'pods') return 'Current pod count at T. Historical records contribute window CPU/memory usage only.';
  return 'Node usage is independently measured, never added to pod totals. The unused complement is not scheduling headroom. Pod usage includes historical pods; requests are current at T.';
}
function panelHead(title, subtitle, size) {
  return `<div class="side-head"><div><h3>${escapeHTML(title)}${size == null ? '' : ` <span class="tag">${escapeHTML(size)}</span>`}</h3><span class="side-subtitle">${escapeHTML(subtitle)}</span></div><button class="clr" onclick="clearSel()">Clear</button></div>`;
}
function technicalDetails(metadata) {
  return `<details class="technical-details"><summary>Technical details</summary><pre>${escapeHTML(JSON.stringify(metadata, null, 2))}</pre></details>`;
}
function resourceCard(res, amount, label, capacity, secondary, help, value = null) {
  const ratio = capacity > 0 && value != null ? Math.max(0, 100 * value / capacity) : null;
  return `<section class="resource-card"><div class="resource-heading">${{ cpu: 'CPU', mem: 'Memory', nic: 'NIC slots', pods: 'Pods' }[res]}${helpButton(help, `${res} resource help`)}</div>`
    + `<div class="resource-value">${amount}</div><div class="resource-label">${label}</div>`
    + (capacity !== undefined ? `<div class="resource-bar${ratio == null ? ' unavailable' : ''}" aria-hidden="true"><i style="width:${Math.min(100, ratio ?? 0)}%"></i></div>`
      + `<div class="resource-capacity">${capacity == null ? 'Capacity unknown' : `${panelAmount(capacity, res)} capacity`}</div>` : '')
    + (secondary ? `<div class="resource-secondary">${secondary}</div>` : '') + '</section>';
}
function podTotal(pods, res, compact = false) {
  const total = pods.reduce((sum, p) => sum + (res === 'pods' && !isObserved() ? 1 : p[key(res)] ?? 0), 0);
  const missing = isObserved() ? pods.filter(p => podIncomplete(p, res)).length : 0;
  const unknown = isObserved() && pods.length > 0 && pods.every(p => p[key(res)] == null);
  const amount = unknown ? (compact ? '&ndash;' : 'Unknown') : panelAmount(total, res);
  const displayed = compact ? amount.replace(/ (vCPU|GiB|NIC|pods)$/, '') : amount;
  return displayed + (missing ? ` ${compact
    ? `<button type="button" class="help-button compact-help" aria-label="${res}: ${missing} incomplete records" data-tip="${missing} records incomplete; known total is a lower bound">*</button>`
    : helpButton(`${missing} of ${pods.length} records have missing/incomplete ${windowUsage(res) ? 'usage coverage' : 'requests'}. Known values only; this total is a lower bound, not zero for missing data.`, `${res}: ${missing} incomplete records`, true)}` : '');
}

// units + labels per resource for the node panel
const RES_UNIT = { cpu: v => (v / 1000).toFixed(1) + ' cores', mem: v => fmt(v / 1024) + ' GiB', nic: v => Math.round(v) + ' NIC', pods: v => Math.round(v) + ' pods' };
const DONUT_SEGS = [
  ['system', COL_INFRA_SYS, 'system-reserved'],
  ['daemonset', COL_INFRA_DS, 'daemonset'],
  ['buffer', COL_INFRA_BUF, 'buffer'],
  ['pods', '#4f9dff', 'HCP pods'],
  ['free', COL_FREE_CELL, 'free'],
];

function resAmount(v, res) {
  return res === 'mem' ? `${fmt(v)} MiB` : res === 'cpu' ? `${fmt(v)} mC`
    : res === 'nic' ? `${(+v).toFixed(v < 1 ? 2 : 0)} NIC` : `${Math.round(v)} pods`;
}

function observedPodTable(pods) {
  return workloadTable(pods);
}
function workloadTable(pods) {
  const groups = new Map();
  for (const p of pods) {
    const name = `${p.component ?? 'Pod'}${p.reserve ? ` (${p.reserve} reserve)` : ''}`;
    if (!groups.has(name)) groups.set(name, []);
    groups.get(name).push(p);
  }
  const rows = [...groups].sort((a, b) => b[1].reduce((sum, p) => sum + (p.mem_mib ?? 0), 0) - a[1].reduce((sum, p) => sum + (p.mem_mib ?? 0), 0)).map(([name, members]) => {
    const individuals = members.map(p => {
      const o = p.observed;
      const placed = isObserved() ? null : curMCs().flatMap(mc => allNodes(mc.packing)).find(n => n.pods.includes(p));
      const node = isObserved() ? o.nodeID : placed ? panelNodeID(placed) : null;
      return `<div class="pod-record"><div class="pod-record-head"><b>${escapeHTML(isObserved() ? o.name : `HCP ${p.hcp} · ${p.tier ?? 'pod'}`)}</b>`
        + (node == null ? '<span class="muted">Unplaced</span>' : S.selNode && panelNodeID(S.selNode) === node ? '' : `<button class="node-link" data-node="${escapeHTML(node)}" aria-label="${escapeHTML(`Go to node ${isObserved() ? o.node : node}`)}">Node ${escapeHTML(node)}</button>`) + '</div>'
        + `<div class="pod-metrics">${podTotal([p], 'cpu')} · ${podTotal([p], 'mem')} · ${panelAmount(p.nic, 'nic')}</div>`
        + (isObserved() ? `<div class="pod-status muted">${escapeHTML(p.label)} · ${escapeHTML(podUsageStatus(p))}</div>` : '')
        + technicalDetails(isObserved() ? o : p) + '</div>';
    }).join('');
    return `<details class="workload-group"><summary><span class="component-name" title="${escapeHTML(name)}">${escapeHTML(name)}</span><span>${members.length}</span><span>${podTotal(members, 'cpu', true)}</span><span>${podTotal(members, 'mem', true)}</span></summary>${individuals}</details>`;
  }).join('');
  return `<div class="workload-heading"><h4>Workloads <span class="muted">${pods.length} records</span></h4><span class="muted">${isObserved() ? (S.policy === 'usage' ? 'Window usage' : 'Requests at T') : 'Requests'}${helpButton(isObserved() ? 'Rows group normalized component names. Count is recorded identities, not current occupancy. Expand for each pod, its provenance and node. CPU/memory totals follow the selected basis.' : 'Rows group component and reserve type. Expand for individual pods and node navigation.', 'Workload totals help')}</span></div>`
    + `<div class="workload-columns"><span>Component</span><span>#</span><span>vCPU</span><span>GiB</span></div>${rows || '<p class="muted">No pod records.</p>'}`;
}

function donutSVG(node, res) {
  if (isObserved() && !(fullCap(node, res) > 0)) {
    return `<svg width="60" height="60" viewBox="0 0 60 60"><circle cx="30" cy="30" r="26" fill="none" stroke="${COL_UNKNOWN}" stroke-width="8"/>
      <text x="30" y="31" text-anchor="middle" font-size="13" fill="#e6ebf2">${res === 'nic' && fullCap(node, res) === 0 ? 'N/A' : '?'}</text>
      <text x="30" y="43" text-anchor="middle" font-size="8" fill="#8b97a8">${res}</text></svg>`;
  }
  const full = fullCap(node, res) || 1;
  const parts = {
    system: val(node.infra.system, res), daemonset: val(node.infra.daemonset, res),
    buffer: val(node.infra.buffer, res), pods: val(node.used, res),
  };
  parts.free = Math.max(0, full - parts.system - parts.daemonset - parts.buffer - parts.pods);
  const R = 26, r = 16, cx = 30, cy = 30, C = 2 * Math.PI * R;
  let off = 0, arcs = '';
  const remainder = isObserved() ? remainderKind(node, res) : 'free';
  const segments = isObserved() ? [
    ['system', COL_INFRA_SYS, 'capacity minus allocatable'], ['pods', '#4f9dff', `known pod total${node.incomplete[key(res)] ? ' (lower bound)' : ''}`],
    ['free', remainder === 'unknown' ? COL_UNKNOWN : COL_FREE_CELL, remainderLabel(remainder)]
  ] : DONUT_SEGS;
  for (const [k, col, label] of segments) {
    const frac = parts[k] / full; if (frac <= 0) continue;
    const len = (isObserved() ? Math.min(frac, Math.max(0, 1 - off / C)) : frac) * C;
    const tip = `${label} · ${resAmount(parts[k], res)} · ${Math.round(frac * 100)}%`;
    arcs += `<circle class="darc" data-tip="${tip}" cx="${cx}" cy="${cy}" r="${R}" fill="none" stroke="${col}" stroke-width="8"
      stroke-dasharray="${len} ${C - len}" stroke-dashoffset="${-off}" transform="rotate(-90 ${cx} ${cy})"/>`;
    off += len;
  }
  const percent = 100 * (isObserved() ? parts.system + parts.pods : full - parts.free) / full;
  const usedPct = isObserved() && node.incomplete[key(res)] ? `≥${Math.floor(percent)}%` : `${Math.round(percent)}%`;
  return `<svg width="60" height="60" viewBox="0 0 60 60">${arcs}
    <text x="30" y="31" text-anchor="middle" font-size="13" fill="#e6ebf2" font-weight="700">${usedPct}</text>
    <text x="30" y="43" text-anchor="middle" font-size="8" fill="#8b97a8">${res}</text></svg>`;
}

function renderNodePanel(side, node) {
  const n = node.observed;
  const cards = ['cpu', 'mem', 'nic', 'pods'].map(res => {
    const cap = fullCap(node, res), measured = isObserved() && (res === 'cpu' || res === 'mem');
    const raw = measured ? n.node_usage?.[key(res)] : val(node.used, res) + (isObserved() ? 0 : infraCells3(node, res).reduce((sum, p) => sum + p.v, 0));
    const value = typeof raw === 'number' && Number.isFinite(raw) && raw >= 0 ? raw : null;
    const na = res === 'nic' && cap === 0;
    const amount = na ? 'N/A' : measured || !isObserved() ? panelAmount(value, res) : podTotal(node.pods, res);
    const label = measured ? 'Measured node usage' : isObserved() ? (res === 'nic' ? 'Requested at T' : 'Current at T') : 'Allocated';
    const secondary = measured ? `<span>Pod ${S.policy}</span> ${podTotal(node.pods, res)}`
      : na ? `<span>Requested</span> ${podTotal(node.pods, res)}` : !isObserved() ? `<span>Pods + reserves</span> ${podTotal(node.pods, res)}` : '';
    const unused = measured && value != null && cap != null ? ` Unused complement: ${panelAmount(Math.max(0, cap - value), res)}${cap > 0 ? ` (${Number((100 * Math.max(0, cap - value) / cap).toFixed(1))}%)` : ''}.` : '';
    return resourceCard(res, amount, label, cap, secondary, resourceHelp(res) + unused, na ? null : value);
  }).join('');
  const metadata = isObserved() ? Object.fromEntries(Object.entries(n).filter(([k]) => k !== 'pods'))
    : { id: node.id, sku: node.sku, capacity: node.full, infrastructure: node.infra, podRequests: node.used };
  side.innerHTML = panelHead(`Node ${panelNodeID(node)}`, isObserved() ? `${n.pool} · ${shortSku(node.sku)} · ${n.current === true ? 'current' : 'historical'}` : shortSku(node.sku))
    + `<div class="resource-grid">${cards}</div>` + technicalDetails(metadata) + workloadTable(node.pods);
}

/* ---------- legend ---------- */
function lgActive(cat, val) {
  const f = S.legendFocus;
  return f && f.cat === cat && (f.val == null ? val == null : f.val === val);
}
function lgItem(color, label, hatch, cat, val) {
  const cls = 'lg lg-click' + (lgActive(cat, val) ? ' active' : '');
  const va = (val != null) ? ` data-val="${escapeHTML(val)}"` : '';
  return `<span class="${cls}" data-cat="${escapeHTML(cat)}"${va}><i class="${hatch ? 'hatched' : ''}" style="background-color:${color}"></i>${escapeHTML(label)}</span>`;
}
function drawLegend() {
  const el = document.getElementById('legend');
  const items = (isObserved() ? [
    lgItem(COL_INFRA_SYS, 'capacity minus allocatable', false, 'kind', 'system'),
    lgItem(COL_UNKNOWN, 'unavailable capacity / requests', true, 'kind', 'unknown'),
    ...(windowUsage(S.resource) ? [lgItem(COL_FREE_CELL, 'outside known pod usage (not necessarily idle)', false, 'kind', 'outside-pods')] : []),
    ...(windowUsage(S.resource) ? [] : [lgItem(COL_FREE_CELL, 'unrequested (complete current data only)', false, 'free', null)]),
    lgItem('#888', 'Non-HCP by namespace', false, 'category', 'non-hcp'),
    lgItem('#888', 'historical / incomplete pods (missing value: 1-cell marker, not measured)', true, 'category', 'historical'),
  ] : [
    lgItem('#232a37', 'system-reserved', false, 'kind', 'system'),
    lgItem('#39435a', 'daemonset overhead', false, 'kind', 'daemonset'),
    lgItem(COL_INFRA_BUF, 'buffer', false, 'kind', 'buffer'),
    lgItem(RESERVE_COL.rollout, 'rollout reserve', true, 'reserve', 'rollout'),
    lgItem(RESERVE_COL.azdeath, 'AZ-death reserve', true, 'reserve', 'azdeath'),
    lgItem(RESERVE_COL.slot, 'reserved slot', true, 'reserve', 'slot'),
    lgItem(COL_FREE_CELL, 'free / unused', false, 'free', null),
  ]).join('');
  const binding = (() => {
    const mc = curMCs()[0]; if (!mc || !mc.zonal_pool) return '';
    const zb = mc.zonal_pool.binding, ob = mc.overflow_pool ? mc.overflow_pool.binding : '–';
    return `binding: zonal <b>${zb}</b> · overflow <b>${ob}</b>`;
  })();
  const grad = (S.color === 'hcp')
    ? '<span class="lg-lbl">HCP by size:</span>' + sizeGrads.map(g =>
        `<span class="lg lg-grad lg-click${lgActive('size', g.size) ? ' active' : ''}" data-cat="size" data-val="${escapeHTML(g.size)}">${escapeHTML(sizeLabel(g.size))}<i style="background:linear-gradient(90deg,${g.c0},${g.c1})"></i></span>`).join('')
    : '';
  const hint = S.legendFocus ? ' <span class="lg-note">· click again to clear</span>' : '';
  el.innerHTML = items + grad
      + `<span class="lg-note">${S.color === 'hcp' ? 'pods colored by HCP size (gradient within size)' : 'pods colored by component'} · 1 cell = ${CELL_UNIT[S.resource]} ${RES_LABEL[S.resource]} · ${isObserved() ? '* / lower bound: incomplete pod totals; [?]: missing node capacity. ' + (windowUsage(S.resource) ? 'Dark remainder includes idle, host and unmeasured pod usage; independent node usage is in node detail, not added to pods.' : 'current at T') : binding}</span>` + hint;
}
function onLegendClick(e) {
  const lg = e.target.closest('.lg-click');
  if (!lg) return;
  const cat = lg.dataset.cat, val = lg.dataset.val != null ? lg.dataset.val : null;
  S.legendFocus = lgActive(cat, val) ? null : { cat, val };   // toggle
  render();
}
function dimColor(c) {
  const m = c.match(/^rgb\((\d+),(\d+),(\d+)\)$/);
  if (m) return `rgba(${m[1]},${m[2]},${m[3]},.14)`;
  return c.startsWith('hsl') ? c.replace(/(\d+)%\)$/, '18%)') : 'rgba(90,100,120,.35)';
}

/* ---------- self-boot: works for direct page loads (/preview) and HTMX swaps (/) ---------- */
function bootTetris() {
  if (document.getElementById('sim-data')?.textContent && document.getElementById('tetris')) initTetris();
}
if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', bootTetris);
else bootTetris();
// Render on both swap and settle: htmx's settle phase can re-insert the swapped
// content (a fresh, unpainted canvas), so an afterSwap-only hook leaves it blank
// on a second solve. Re-initialising on afterSettle keeps the canvas painted.
document.addEventListener('htmx:afterSwap', bootTetris);
document.addEventListener('htmx:afterSettle', bootTetris);
