// Browser-only test helper. The dashboard itself never uses a network API.
import { spawn } from "node:child_process";
import { writeFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";
import path from "node:path";

const [chrome, html, profile, artifactDirectory, mode] = process.argv.slice(2);
if (mode && mode !== "overview") throw new Error(`Unknown review mode: ${mode}`);
const browser = spawn(chrome, ["--headless", "--no-sandbox", "--disable-gpu", "--no-first-run", "--disable-background-networking", "--disable-dev-shm-usage", `--user-data-dir=${profile}`, "--remote-debugging-port=0", "about:blank"], { stdio: ["ignore", "ignore", "pipe"] });
const timeout = setTimeout(() => { browser.kill("SIGKILL"); process.exitCode = 1; }, 45000);
let socket;
try {
  const endpoint = await new Promise((resolve, reject) => {
    let stderr = "";
    browser.stderr.on("data", chunk => {
      stderr += chunk;
      const match = stderr.match(/DevTools listening on (ws:\/\/[^\s]+)/);
      if (match) resolve(match[1]);
    });
    browser.on("error", reject);
    browser.on("exit", code => reject(new Error(`Chrome exited ${code}: ${stderr}`)));
  });
  socket = new WebSocket(endpoint);
  await new Promise((resolve, reject) => { socket.onopen = resolve; socket.onerror = reject; });
  let nextID = 0;
  let session;
  const pending = new Map();
  const errors = [];
  const requests = [];
  socket.onmessage = event => {
    const message = JSON.parse(event.data);
    if (message.id) {
      const promise = pending.get(message.id);
      pending.delete(message.id);
      if (message.error) promise.reject(new Error(JSON.stringify(message.error)));
      else promise.resolve(message.result);
    }
    if (message.method === "Runtime.exceptionThrown") errors.push(JSON.stringify(message.params.exceptionDetails));
    if (message.method === "Runtime.consoleAPICalled" && message.params.type === "error") errors.push(JSON.stringify(message.params.args));
    if (message.method === "Network.requestWillBeSent" && !message.params.request.url.startsWith("file:")) requests.push(message.params.request.url);
  };
  const command = (method, params = {}, sessionId = session) => new Promise((resolve, reject) => {
    const id = ++nextID;
    pending.set(id, { resolve, reject });
    socket.send(JSON.stringify({ id, method, params, sessionId }));
  });
  const target = await command("Target.createTarget", { url: "about:blank" });
  session = (await command("Target.attachToTarget", { targetId: target.targetId, flatten: true })).sessionId;
  await command("Page.enable");
  if (mode === "overview") await command("Page.bringToFront");
  await command("Runtime.enable");
  if (mode === "overview") {
    await command("Network.enable");
    await command("Network.emulateNetworkConditions", { offline: true, latency: 0, downloadThroughput: 0, uploadThroughput: 0 });
  }
  const evaluate = async expression => {
    const result = await command("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
    if (result.exceptionDetails) throw new Error(JSON.stringify(result.exceptionDetails));
    return result.result.value;
  };
  const check = async (name, expression) => {
    if (!await evaluate(expression)) throw new Error(name);
  };
  const screenshot = async name => {
    if (!artifactDirectory) return;
    const result = await command("Page.captureScreenshot", { format: "png", captureBeyondViewport: false });
    await writeFile(path.join(artifactDirectory, `${name}.png`), Buffer.from(result.data, "base64"));
  };
  for (const [name, width, height] of [["desktop", 1280, 1000], ["mobile", 390, 844]]) {
    await command("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 1, mobile: name === "mobile" });
    await command("Page.navigate", { url: pathToFileURL(html).href });
    for (let attempt = 0; attempt < 100; attempt++) {
      if (await evaluate(mode === "overview" ? "document.readyState === 'complete' && !!document.querySelector('.comparison')" : "document.readyState === 'complete' && !!document.querySelector('.envelope-point')")) break;
      await new Promise(resolve => setTimeout(resolve, 30));
    }
    if (mode === "overview") {
      await check(`${name}: exact viewport`, `innerWidth === ${width}`);
      await check(`${name}: no overflow`, "document.documentElement.scrollWidth <= innerWidth");
      await check(`${name}: all summaries visible`, "document.querySelectorAll('.size-summary').length === 3 && [...document.querySelectorAll('.size-summary')].every(e => e.getBoundingClientRect().height > 0)");
      await check(`${name}: complete register`, "document.querySelectorAll('details.run').length === 37 && document.querySelectorAll('.pane').length === 6");
      await check(`${name}: selected comparison axes`, "document.querySelectorAll('.comparison thead th').length === 8 && document.querySelector('.comparison').textContent.includes('Observation hour') && document.querySelector('.comparison').textContent.includes('Larger / full failed hour')");
      await screenshot(`overview-${name}-top`);
      if (name === "mobile") {
        for (const size of ["size100", "medium"]) {
          await evaluate(`document.getElementById('summary-${size}').scrollIntoView({block:'start'})`);
          await screenshot(`overview-${name}-${size}`);
        }
      }
      await evaluate("document.getElementById('comparison').scrollIntoView({block:'start'})");
      await screenshot(`overview-${name}-comparison`);
      await evaluate("document.querySelector('.reference-point').click(); new Promise(resolve => setTimeout(resolve, 50))");
      await check(`${name}: deep link opens and focuses run`, "document.querySelector('details.run[open]').textContent.includes('envelope-observe5-hour-004') && document.activeElement.tagName === 'SUMMARY'");
      await command("Input.dispatchKeyEvent", { type: "keyDown", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13, text: "\r" });
      await command("Input.dispatchKeyEvent", { type: "keyUp", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13 });
      await check(`${name}: keyboard closes details`, "!document.querySelector('details.run[open]')");
      await command("Input.dispatchKeyEvent", { type: "keyDown", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13, text: "\r" });
      await command("Input.dispatchKeyEvent", { type: "keyUp", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13 });
      await check(`${name}: keyboard opens details`, "!!document.querySelector('details.run[open]')");
      await check(`${name}: open details no overflow`, "document.documentElement.scrollWidth <= innerWidth");
      await screenshot(`overview-${name}-run`);
      await evaluate("document.querySelectorAll('details').forEach(d => d.open = true)");
      await check(`${name}: all expanded no overflow`, "document.documentElement.scrollWidth <= innerWidth");
      await check(`${name}: zero subresources`, "performance.getEntriesByType('resource').length === 0");
      await command("Emulation.setScriptExecutionDisabled", { value: true });
      await command("Page.navigate", { url: pathToFileURL(html).href });
      await new Promise(resolve => setTimeout(resolve, 250));
      await check(`${name}: no-JS useful overview`, "document.querySelectorAll('.size-summary').length === 3 && document.querySelectorAll('details.run').length === 37 && document.querySelector('.assessment-notes').textContent.includes('5.477778%')");
      await evaluate("document.querySelector('details.run summary').focus()");
      await command("Input.dispatchKeyEvent", { type: "keyDown", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13, text: "\r" });
      await command("Input.dispatchKeyEvent", { type: "keyUp", key: "Enter", code: "Enter", windowsVirtualKeyCode: 13 });
      await check(`${name}: no-JS keyboard disclosure`, "document.querySelector('details.run').open");
      await evaluate("scrollTo(0,0)");
      await screenshot(`overview-${name}-nojs`);
      await command("Emulation.setScriptExecutionDisabled", { value: false });
      if (requests.length) throw new Error(`Unexpected network attempts: ${requests.join(', ')}`);
      console.log(`${name}: overview / 37 runs / keyboard / no-JS / no overflow / zero network attempts at ${width}x${height}`);
      continue;
    }
    await check(`${name}: page ready`, "!document.getElementById('workspace').hidden");
    await check(`${name}: exact viewport`, `innerWidth === ${width}`);
    await check(`${name}: initial overflow`, "document.documentElement.scrollWidth <= innerWidth");
    if (await evaluate("JSON.parse(document.getElementById('dashboard-data').textContent).sizes[0].id === 'size100'")) {
      await check(`${name}: Size100 distinct default`, "document.getElementById('selected-run').value === 'size100-q15' && document.querySelector('h1').textContent === 'Size100'");
      await check(`${name}: measured bounded cases`, "document.querySelectorAll('.envelope-point').length === 6 && document.querySelectorAll('.envelope-point.met').length === 3 && document.querySelectorAll('.envelope-point.exceeded[data-shape=\"interrupted\"]').length === 3");
      await check(`${name}: separate economic and historical inventory`, "document.querySelector('.profile-details').textContent.includes('September22:49=47base+2') && document.querySelector('.profile-details').textContent.includes('CompletedJobs notrunningcharge') && document.querySelector('.cost-summary').textContent.includes('$0.2498600')");
      await evaluate("document.querySelector('.envelope-point[data-run=\"size100-p600-q15\"]').dispatchEvent(new MouseEvent('click',{bubbles:true}))");
      await check(`${name}: 600 cohort versus cutoff`, "document.getElementById('run-notes').textContent.includes('106/900') && document.getElementById('run-notes').textContent.includes('entirelycanceledOffers1 atcutoff') && document.getElementById('request-outcomes').textContent.includes('generator limited 1') && document.getElementById('run-metadata').textContent.includes('600 pods / 120 applications / 16 guest nodes')");
      await evaluate("document.querySelector('.envelope-point[data-run=\"size100-p1200-q15\"]').dispatchEvent(new MouseEvent('click',{bubbles:true}))");
      await check(`${name}: 1200 complete seed then rejection`, "document.getElementById('run-notes').textContent.includes('86/900') && document.getElementById('run-notes').textContent.includes('Fullseed completed') && document.getElementById('request-outcomes').textContent.includes('934') && document.getElementById('run-metadata').textContent.includes('1200 pods / 240 applications / 16 guest nodes')");
      await screenshot(`size100-${name}-population`);
      await evaluate("document.querySelector('.envelope-point[data-run=\"size100-q15\"]').dispatchEvent(new MouseEvent('click',{bubbles:true}))");
      await screenshot(`size100-${name}-overview`);
      await evaluate("document.querySelector('.envelope-point[data-run=\"size100-q20\"]').dispatchEvent(new MouseEvent('click',{bubbles:true}))");
      await check(`${name}: rejection versus final drain`, "document.getElementById('run-notes').textContent.includes('76/1200') && document.getElementById('request-outcomes').textContent.includes('1,235') && document.getElementById('request-outcomes').textContent.includes('canceled 8') && document.getElementById('selected-badges').textContent.includes('Request quality: failed')");
      await check(`${name}: explicit topology`, "document.getElementById('run-metadata').textContent.includes('300 pods / 60 applications / 16 guest nodes')");
      await evaluate("document.querySelector('.envelope-point[data-run=\"size100-q15\"]').dispatchEvent(new MouseEvent('click',{bubbles:true}))");
      for (const tab of ['latency', 'composition', 'resources']) {
        await evaluate(`document.getElementById('tab-${tab}').click(); document.getElementById('panel-${tab}').scrollIntoView({block:'start'})`);
        await check(`${name}: Size100 ${tab} no overflow`, "document.documentElement.scrollWidth <= innerWidth");
        await screenshot(`size100-${name}-${tab}`);
      }
      await check(`${name}: measured resource composition`, "document.querySelectorAll('.tile').length > 0");
      await evaluate("const search=document.getElementById('component-search'); search.value='kube-apiserver'; search.dispatchEvent(new Event('input')); const fold=document.querySelector('.component-distribution'); fold.open=true; fold.dispatchEvent(new Event('toggle'))");
      await check(`${name}: Size100 resource boxes`, "document.querySelectorAll('#resource-charts svg rect').length > 0");
      await check(`${name}: finite SVG`, "[...document.querySelectorAll('svg *')].every(node=>[...node.attributes].every(attr=>!/NaN|Infinity/.test(attr.value)))");
      if (name === 'mobile') {
        await evaluate("document.getElementById('nav-toggle').click()");
        await check('Size100 mobile navigation', "document.getElementById('nav-toggle').getAttribute('aria-expanded') === 'true'");
        await evaluate("document.getElementById('navigation').dispatchEvent(new KeyboardEvent('keydown',{key:'Escape',bubbles:true}))");
      }
      await check(`${name}: no external requests`, "performance.getEntriesByType('resource').length === 0");
      console.log(`${name}: Size100 saved-data browser checks passed at ${width}x${height}`);
      continue;
    }
    if (await evaluate("JSON.parse(document.getElementById('dashboard-data').textContent).sizes.some(size => size.id === 'medium')")) {
      await check(`${name}: failed full hour default`, "document.getElementById('selected-run').value === 'medium-2400-q40-v8c-hour' && document.getElementById('selected-badges').textContent.includes('Execution: failed') && document.getElementById('selected-badges').textContent.includes('Request quality: failed')");
      await check(`${name}: not early screen or generator-inconclusive`, "document.querySelectorAll('.envelope-point.exceeded[data-shape=\"hour\"]').length === 1 && document.querySelectorAll('.envelope-point').length === 1 && document.getElementById('request-outcomes').textContent.includes('144,000')");
      await check(`${name}: turnover and recovery causes retained`, "document.getElementById('run-metadata').textContent.includes('474 / 480') && document.getElementById('run-notes').textContent.includes('client rate limiter Wait') && document.getElementById('run-notes').textContent.includes('canaryFailures=1') && document.getElementById('run-notes').textContent.includes('recovered=false')");
      await check(`${name}: changed management topology explicit`, "document.querySelector('#medium-six-worker-hour .profile-details').textContent.includes('2/2/2') && document.querySelector('#medium-six-worker-hour .profile-details').textContent.includes('leak unresolved')");
      await screenshot(`medium-${name}-q40-hour`);
      await evaluate("location.hash = 'medium-capacity-v7'; new Promise(resolve => setTimeout(resolve, 30))");
      await check(`${name}: quick campaign default`, "document.getElementById('selected-run').value === 'medium-capacity-p2400-q45'");
      await check(`${name}: delivered server failures distinct`, "document.querySelectorAll('.envelope-point').length === 5 && document.querySelectorAll('.envelope-point.met').length === 2 && document.querySelectorAll('.envelope-point.exceeded[data-shape=\"interrupted\"]').length === 3");
      await check(`${name}: checkpoint vs final cohort`, "document.getElementById('run-notes').textContent.includes('144/2700') && document.getElementById('request-outcomes').textContent.includes('2,714') && document.getElementById('run-metadata').textContent.includes('2400 pods / 480 applications / 16 guest nodes')");
      await check(`${name}: bounded duration and failure`, "document.getElementById('selected-badges').textContent.includes('Request quality: failed') && document.getElementById('run-notes').textContent.includes('fullWindow=false')");
      await check(`${name}: no invented runtime usage`, "JSON.parse(document.getElementById('dashboard-data').textContent).sizes.find(s=>s.id==='medium-capacity-v7').runs.every(r=>r.wholeHCP[0].distribution === null)");
      await screenshot(`medium-${name}-capacity`);
      await evaluate("document.querySelector('.envelope-point[data-run=\"medium-capacity-p1200-q45\"]').dispatchEvent(new MouseEvent('click',{bubbles:true}))");
      await check(`${name}: accepted180s point`, "document.getElementById('selected-badges').textContent.includes('Request quality: passed') && document.getElementById('run-notes').textContent.includes('Full180s')");
      await evaluate("location.hash = 'medium-scheduler100'; new Promise(resolve => setTimeout(resolve, 30))");
      await check(`${name}: revised hour default`, "document.getElementById('selected-run').value === 'medium-scheduler100-v6-hour' && document.getElementById('selected-badges').textContent.includes('Request quality: passed')");
      await check(`${name}: revised measured cases`, "document.querySelectorAll('.envelope-point').length === 4 && document.querySelectorAll('.envelope-point.met').length === 3 && document.querySelectorAll('.envelope-point[data-pods=\"1200\"]').length === 4");
      await check(`${name}: V5 hour inconclusive not interrupted`, "document.querySelector('.envelope-point[data-run=\"medium-scheduler100-v5-hour\"]').classList.contains('unknown') && document.querySelector('.envelope-point[data-run=\"medium-scheduler100-v5-hour\"]').dataset.shape === 'hour'");
      await check(`${name}: V6 counts`, "document.getElementById('request-outcomes').textContent.includes('108,000') && document.getElementById('run-metadata').textContent.includes('240 / 240')");
      await screenshot(`medium-${name}-revised-hour`);
      await evaluate("document.getElementById('tab-composition').click()");
      await check(`${name}: hour partial resource measurements present`, "document.querySelectorAll('.tile').length > 0");
      await evaluate("location.hash = 'medium'; new Promise(resolve => setTimeout(resolve, 30))");
      await check(`${name}: Medium interrupted reference`, "document.getElementById('selected-run').value === 'medium-hour20' && document.getElementById('selected-badges').textContent.includes('UNUSABLE')");
      await check(`${name}: seven screens and diagnostic hour`, "document.querySelectorAll('.envelope-point').length === 8 && document.querySelectorAll('.envelope-point.met').length === 7");
      await check(`${name}: population lanes not pooled`, "document.querySelectorAll('.envelope-point[data-pods=\"300\"]').length === 7 && document.querySelectorAll('.envelope-point[data-pods=\"600\"]').length === 1 && !document.querySelector('.envelope-point[data-pods=\"1200\"]')");
      await check(`${name}: seed failure not an API result`, "document.querySelector('#medium .profile-details').textContent.includes('1200-Pod attempt: NOT MEASURED') && document.querySelector('#medium .profile-details').textContent.includes('1070 Ready Pods')");
      await check(`${name}: interruption not capacity ceiling`, "document.querySelector('.envelope-point[data-run=\"medium-hour20\"]').dataset.shape === 'interrupted' && document.querySelector('.envelope-point[data-run=\"medium-hour20\"]').classList.contains('unknown')");
      await screenshot(`medium-${name}-overview`);
      await evaluate("document.getElementById('tab-composition').click()");
      await check(`${name}: failed queries not zero usage`, "document.querySelectorAll('.tile').length === 0 && document.getElementById('treemaps').textContent.includes('unavailable')");
      await evaluate("const picker = document.getElementById('selected-run'); picker.value = 'medium-screen30-p600'; picker.dispatchEvent(new Event('change'))");
      await check(`${name}: selected population explicit`, "document.getElementById('selected-context').textContent.includes('600 Pods / 120 apps / 6 nodes') && document.getElementById('run-metadata').textContent.includes('600 pods / 120 applications / 6 guest nodes') && document.getElementById('run-metadata').textContent.includes('10 / 10')");
      await check(`${name}: non-JS population explicit`, "document.querySelector('#medium .run-register').textContent.includes('600 Pods / 120 apps / 6 nodes')");
      await check(`${name}: measured screen resources`, "document.querySelectorAll('.tile').length > 0");
      for (const tab of ['latency', 'composition', 'resources']) {
        await evaluate(`document.getElementById('tab-${tab}').click(); document.getElementById('panel-${tab}').scrollIntoView({block:'start'})`);
        await check(`${name}: ${tab} no overflow`, "document.documentElement.scrollWidth <= innerWidth");
        await screenshot(`medium-${name}-${tab}`);
      }
      await evaluate("const search = document.getElementById('component-search'); search.value = 'kube-apiserver'; search.dispatchEvent(new Event('input')); const fold = document.querySelector('.component-distribution'); fold.open = true; fold.dispatchEvent(new Event('toggle'))");
      await check(`${name}: resource boxes`, "document.querySelectorAll('#resource-charts svg rect').length > 0");
      await check(`${name}: finite SVG`, "[...document.querySelectorAll('svg *')].every(node => [...node.attributes].every(attr => !/NaN|Infinity/.test(attr.value)))");
      if (name === 'mobile') {
        await evaluate("document.getElementById('nav-toggle').click()");
        await check('Medium mobile navigation', "document.getElementById('nav-toggle').getAttribute('aria-expanded') === 'true'");
        await evaluate("document.getElementById('navigation').dispatchEvent(new KeyboardEvent('keydown', {key:'Escape', bubbles:true}))");
      }
      await check(`${name}: no external requests`, "performance.getEntriesByType('resource').length === 0");
      console.log(`${name}: Medium saved-data browser checks passed at ${width}x${height}`);
      continue;
    }
    await check(`${name}: actual reference hour`, "document.getElementById('selected-run').value === 'envelope-observe5-hour-004'");
    await check(`${name}: reference not qualification`, "document.querySelector('.reference-summary').textContent.includes('Reference hour attempt') && document.querySelector('.reference-summary .badge.fail').textContent.includes('Health: failed')");
    await check(`${name}: default cohort`, "document.getElementById('cohort').value === 'matched-pinned-envelope' && document.getElementById('kind').value === 'screen'");
    await check(`${name}: all measured points`, "document.querySelectorAll('.envelope-point').length === JSON.parse(document.getElementById('dashboard-data').textContent).sizes[0].runs.length");
    await check(`${name}: request failed independent of execution`, "document.querySelector('.envelope-point[data-run=\"envelope-unloaded15-001\"]').classList.contains('exceeded') && document.querySelector('.envelope-point[data-run=\"envelope-unloaded15-001\"]').dataset.shape === 'screen'");
    await check(`${name}: interrupted shape`, "document.querySelector('.envelope-point[data-run=\"envelope-loaded5-hour-001\"]').dataset.shape === 'interrupted'");
    await check(`${name}: finite positive boxes`, "[...document.querySelectorAll('#latency-charts svg rect')].every(rect => Number.isFinite(+rect.getAttribute('width')) && +rect.getAttribute('width') > 0)");
    await screenshot(`${name}-overview`);
    await evaluate("document.getElementById('envelope-chart').scrollIntoView({block:'center'})");
    await screenshot(`${name}-envelope`);
    await evaluate("document.getElementById('panel-latency').scrollIntoView({block:'start'})");
    await screenshot(`${name}-latency`);
    await evaluate("document.getElementById('tab-composition').click(); document.getElementById('panel-composition').scrollIntoView({block:'start'})");
    await check(`${name}: measured treemaps`, "document.querySelectorAll('.tile').length > 0 && document.getElementById('percentile-output').textContent === 'p75'");
    await check(`${name}: composition overflow`, "document.documentElement.scrollWidth <= innerWidth");
    await check(`${name}: tile bounds`, "[...document.querySelectorAll('.tile')].every(tile => {const box = tile.getBoundingClientRect(), parent = tile.parentElement.getBoundingClientRect(); return box.right <= parent.right + 1 && box.bottom <= parent.bottom + 1;})");
    await screenshot(`${name}-composition`);
    await evaluate("document.querySelector('.tile').click()");
    await check(`${name}: component details`, "document.activeElement.id === 'component-detail' && document.getElementById('component-detail').textContent.includes('absolute component totals')");
    await evaluate("const slider = document.getElementById('percentile'); slider.value = '2'; slider.dispatchEvent(new Event('input'))");
    await check(`${name}: percentile update`, "document.getElementById('percentile-output').textContent === 'p99'");
    await evaluate("const runSlider = document.getElementById('run-slider'); runSlider.value = '1'; runSlider.dispatchEvent(new Event('input'))");
    await check(`${name}: repeated run selection`, "document.getElementById('selected-run').value === 'envelope-loaded4-002'");
    await evaluate("const picker = document.getElementById('selected-run'); picker.value = 'cpo144-pinned-hour3-001'; picker.dispatchEvent(new Event('change'))");
    await check(`${name}: legacy policy`, "document.getElementById('request-outcomes').textContent.includes('Legacy strict policy') && !document.getElementById('request-outcomes').textContent.includes('Threshold 0%')");
    await evaluate("const strict = document.querySelector('.envelope-point[data-run=\"envelope-loaded5-hour-001\"]'); strict.dispatchEvent(new KeyboardEvent('keydown', {key:'Enter', bubbles:true}))");
    await check(`${name}: strict hard failure`, "document.getElementById('selected-run').value === 'envelope-loaded5-hour-001' && document.getElementById('incidents').firstElementChild.textContent === '1Hard canary failures'");
    await evaluate("document.querySelector('.envelope-point[data-run=\"envelope-loaded4-001\"]').dispatchEvent(new MouseEvent('click', {bubbles:true})); document.getElementById('tab-resources').click(); const search = document.getElementById('component-search'); search.value = 'kube-apiserver'; search.dispatchEvent(new Event('input')); const fold = document.querySelector('.component-distribution'); fold.open = true; fold.dispatchEvent(new Event('toggle')); document.getElementById('panel-resources').scrollIntoView({block:'start'})");
    await check(`${name}: resource boxes`, "document.querySelectorAll('#resource-charts svg rect').length > 0");
    await check(`${name}: resource overflow`, "document.documentElement.scrollWidth <= innerWidth");
    await screenshot(`${name}-resources`);
    await evaluate("document.getElementById('tab-resources').dispatchEvent(new KeyboardEvent('keydown', {key:'Home', bubbles:true}))");
    await check(`${name}: keyboard tabs`, "document.getElementById('tab-latency').getAttribute('aria-selected') === 'true'");
    await evaluate("location.hash = 'Medium'; new Promise(resolve => setTimeout(resolve, 20))");
    await check(`${name}: no fabricated medium`, "!document.getElementById('placeholder').hidden && document.getElementById('workspace').hidden && document.getElementById('placeholder-copy').textContent.includes('No measured results')");
    await evaluate("location.hash = 'methodology'; new Promise(resolve => setTimeout(resolve, 20))");
    await check(`${name}: documentation shell`, "document.getElementById('placeholder-title').textContent === 'Methodology'");
    await evaluate("location.hash = 'small'; new Promise(resolve => setTimeout(resolve, 20))");
    if (name === "mobile") {
      await evaluate("document.getElementById('nav-toggle').click()");
      await check("mobile: nav opens", "document.getElementById('nav-toggle').getAttribute('aria-expanded') === 'true'");
      await evaluate("document.getElementById('navigation').dispatchEvent(new KeyboardEvent('keydown', {key:'Escape', bubbles:true}))");
      await check("mobile: local envelope scroll", "document.getElementById('envelope-chart').scrollWidth > document.getElementById('envelope-chart').clientWidth && document.documentElement.scrollWidth <= innerWidth");
    }
    await check(`${name}: finite SVG`, "[...document.querySelectorAll('svg *')].every(node => [...node.attributes].every(attr => !/NaN|Infinity/.test(attr.value)))");
    await check(`${name}: no external requests`, "performance.getEntriesByType('resource').length === 0");
    console.log(`${name}: saved-data browser checks passed at ${width}x${height}`);
  }
  if (errors.length) throw new Error(`Browser errors: ${errors.join('\n')}`);
} finally {
  socket?.close();
  browser.kill("SIGTERM");
  clearTimeout(timeout);
}
