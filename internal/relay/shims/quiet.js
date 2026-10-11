(() => {
  if (window.__agentBridgeQuiet) return; window.__agentBridgeQuiet = 1;
  const quiet = (el) => { el.setAttribute("data-1p-ignore", ""); el.setAttribute("data-lpignore", "true"); el.setAttribute("data-bwignore", ""); };
  new MutationObserver((ms) => { for (const m of ms) for (const n of m.addedNodes) if (n.nodeType === 1) {
    if (/^(INPUT|TEXTAREA|SELECT|FORM)$/.test(n.tagName)) quiet(n); if (n.querySelectorAll) n.querySelectorAll("input, textarea, select, form").forEach(quiet); } })
    .observe(document, { childList: true, subtree: true });
})();
