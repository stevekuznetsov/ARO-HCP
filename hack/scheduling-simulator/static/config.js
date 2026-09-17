/* HCP scheduling simulator — configuration stepper + decisions summary. */
'use strict';

const STEPS = ['fleet', 'demand', 'sharding', 'headroom', 'capacity', 'topology'];
const STEP_TITLE = { fleet: 'Fleet', demand: 'Demand', sharding: 'Sharding',
  headroom: 'Headroom', capacity: 'Node capacity', topology: 'Topology' };
let curStep = 0;
const seen = new Set();

function $(s, r = document) { return r.querySelector(s); }
function $all(s, r = document) { return [...r.querySelectorAll(s)]; }
function form() { return $('#cfgform'); }
function fv(name) { const el = form().elements[name]; return el ? el.value : ''; }
function pct(x) { return Math.round(parseFloat(x) * 100) + '%'; }
function num(x) { return (+x).toLocaleString(); }

/* ---------- stepper ---------- */
function showStep(i) {
  curStep = Math.max(0, Math.min(STEPS.length - 1, i));
  const step = STEPS[curStep];
  seen.add(step);
  $all('.panel').forEach(p => { p.hidden = p.dataset.step !== step; });
  $all('.step').forEach(b => {
    b.classList.toggle('active', b.dataset.step === step);
    b.classList.toggle('seen', seen.has(b.dataset.step));
  });
  $('#prevStep').disabled = curStep === 0;
  const last = curStep === STEPS.length - 1;
  $('#nextStep').style.visibility = last ? 'hidden' : 'visible';
  const runBtn = $('#runBtn');
  runBtn.classList.toggle('ready', seen.size === STEPS.length);
  $('#stepProgress').textContent = `step ${curStep + 1} of ${STEPS.length} · ${STEP_TITLE[step]}`;
}

function wireStepper() {
  $all('.step').forEach((b, i) => b.addEventListener('click', () => showStep(STEPS.indexOf(b.dataset.step))));
  $('#prevStep').addEventListener('click', () => showStep(curStep - 1));
  $('#nextStep').addEventListener('click', () => showStep(curStep + 1));
  showStep(0);
}

/* ---------- decisions summary ---------- */
function fleetSummary() {
  let total = 0; const mix = [];
  $all('input[name^="count_"]', form()).forEach(el => {
    const n = +el.value || 0; if (n > 0) { total += n; mix.push(`${el.name.slice(6)}n×${n}`); }
  });
  return { value: `${num(total)} HCPs`, detail: mix.join('  ') || 'empty fleet' };
}
function decisionChips() {
  const f = fleetSummary();
  return [
    { step: 'fleet', label: 'Fleet', value: f.value, detail: f.detail },
    { step: 'demand', label: 'Demand', value: `p${fv('percentile')} × ${fv('multiplier')}`,
      detail: 'usage → request transform' },
    { step: 'sharding', label: 'Sharding',
      value: `${fv('hcps_per_mc') - fv('reserve_slots')}+${fv('reserve_slots')} per MC`,
      detail: `${fv('hcps_per_mc')} cap · ${fv('reserve_slots')} reserved ${fv('reserve_size')}-node slots` },
    { step: 'headroom', label: 'Headroom',
      value: `AZ ${pct(fv('az_failure_reserve'))} · rollout ${fv('concurrent_rolling_hcps')} HCP`,
      detail: 'reserved as packable pods' },
    { step: 'capacity', label: 'Node capacity',
      value: `${fv('reservation_mode')} reserve · buf mem ${pct(fv('buffer_mem'))}`,
      detail: `sys ${num(fv('system_reserved_cpu_mc'))}m/${num(fv('system_reserved_mem_mib'))}Mi + daemonset ${num(fv('node_overhead_cpu_mc'))}m/${num(fv('node_overhead_mem_mib'))}Mi · ${fv('max_pods_per_node')} pods/node` },
    { step: 'topology', label: 'Topology',
      value: `overflow ${fv('overflow_az_count')} AZ · unsteered→${fv('unsteered_placement')}`,
      detail: 'zonal is an AZ-balanced triplet' },
  ];
}
function renderDecisions() {
  const el = $('#decisions');
  const chips = decisionChips().map(c =>
    `<button type="button" class="chip" data-step="${c.step}" title="${c.detail}">
       <span class="k">${c.label}</span><span class="v">${c.value}</span></button>`).join('');
  el.innerHTML = `<div class="dec-chips">${chips}</div>
    <button type="button" id="editCfg" class="edit-btn">Edit inputs</button>`;
  $all('.chip', el).forEach(b => b.addEventListener('click', () => { expandConfig(); showStep(STEPS.indexOf(b.dataset.step)); }));
  $('#editCfg').addEventListener('click', () => { expandConfig(); showStep(curStep); });
}

/* ---------- collapse / expand ---------- */
let _editSnapshot = null;
function snapshotForm() {
  const snap = {};
  $all('input, select', form()).forEach(el => { snap[el.name] = el.value; });
  return snap;
}
function restoreForm(snap) {
  if (!snap) return;
  $all('input, select', form()).forEach(el => { if (el.name in snap) el.value = snap[el.name]; });
}
function collapseConfig() {
  $('#config').classList.remove('editing');
  $('#config').classList.add('collapsed');
  document.body.classList.remove('editing');
  document.body.classList.add('solved');
  renderDecisions();
  $('#decisions').hidden = false;
}
function expandConfig() {
  _editSnapshot = snapshotForm();            // so Cancel can discard edits
  $('#config').classList.remove('collapsed');
  $('#config').classList.add('editing');
  document.body.classList.add('editing');    // blur/grey the stale results + decisions
  $('#config').scrollIntoView({ behavior: 'smooth', block: 'start' });
}
function cancelEdit() {
  restoreForm(_editSnapshot);                // discard any edits made this session
  _editSnapshot = null;
  collapseConfig();                          // restore the (still-valid) prior results view
}

/* ---------- validation ---------- */
// A `required`/out-of-range field inside a display:none stepper panel blocks
// submission but the browser can't show its message (not focusable) -> silent.
// The `invalid` event still fires on each bad control; reveal the step of the
// first one so the browser's message lands on a visible field.
function wireValidation() {
  let switched = false;
  form().addEventListener('invalid', e => {
    if (switched) return;                       // only jump to the first invalid field
    switched = true;
    setTimeout(() => { switched = false; }, 0);
    const panel = e.target.closest('.panel');
    if (panel && panel.hidden) showStep(STEPS.indexOf(panel.dataset.step));
  }, true);                                     // capture: `invalid` does not bubble
}

/* ---------- solving indicator ---------- */
const SOLVE_MSGS = [
  'Reading measured control-plane usage…',
  'Applying the usage → request percentile transform…',
  'Expanding the fleet into pods…',
  'Distributing clusters across management clusters…',
  'Reserving rollout-surge and AZ-death headroom…',
  'Holding back reserved growth slots…',
  'Packing zonal pools (balanced AZ triplet)…',
  'Honoring SWIFT-NIC and pods-per-node limits…',
  'Right-sizing a heterogeneous SKU mix…',
  'Resolving the zonal / overflow quota split…',
  'Packing the overflow pool…',
  'Comparing legacy vs minimal…',
];
let _solveTimer = null;
function startSolveMsgs() {
  const el = $('#solvingMsg');
  if (!el) return;
  let i = 0;
  el.textContent = SOLVE_MSGS[0];
  clearInterval(_solveTimer);
  _solveTimer = setInterval(() => {
    i = (i + 1) % SOLVE_MSGS.length;
    el.style.opacity = 0;
    setTimeout(() => { el.textContent = SOLVE_MSGS[i]; el.style.opacity = 1; }, 150);
  }, 2200);
}
function stopSolveMsgs() { clearInterval(_solveTimer); _solveTimer = null; }

/* ---------- boot ---------- */
function bootConfig() {
  if (!$('#cfgform')) return;
  wireStepper();
  wireValidation();
  $('#cancelEdit').addEventListener('click', cancelEdit);
  form().addEventListener('htmx:beforeRequest', startSolveMsgs);
  form().addEventListener('htmx:afterRequest', stopSolveMsgs);
  // collapse config + show the decisions summary once results arrive
  document.body.addEventListener('htmx:afterSwap', e => {
    if (e.target && e.target.id === 'results') collapseConfig();
  });
}
if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', bootConfig);
else bootConfig();
