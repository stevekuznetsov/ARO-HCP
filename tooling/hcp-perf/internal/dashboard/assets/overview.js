// Optional deep-link enhancement. All summaries and disclosures work without JS.
(() => {
  "use strict";
  const reveal = () => {
    let id;
    try { id = decodeURIComponent(location.hash.slice(1)); } catch { return; }
    const run = document.getElementById(id);
    if (!run?.matches("details.run")) return;
    run.open = true;
    run.querySelector("summary").focus({ preventScroll: true });
    run.scrollIntoView({ block: "start" });
  };
  window.addEventListener("hashchange", reveal);
  reveal();
})();
