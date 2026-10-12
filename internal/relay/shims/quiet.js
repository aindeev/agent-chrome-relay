(() => {
  if (window.__agentBridgeQuiet) return; window.__agentBridgeQuiet = 1;
  // Installed after the page had loaded: the page ran a while without Chrome Relay's scripts (it loaded while Chrome
  // had dropped the debugger). The relay then reloads it, so the passkey script is in place before the page's own.
  window.__agentBridgeLate = document.readyState !== "loading" && location.href !== "about:blank";
  // Password managers stay out of agent tabs: Chrome drops Chrome Relay's debugger from a page as soon as another
  // extension puts its own frame in it (1Password's inline menu, say). So: fields are marked for them to ignore,
  // including fields inside the page's own web components; and anything another extension inserts (1Password's
  // <com-1password-*> elements, chrome-extension:// frames) is removed the moment it appears, before its frame loads.
  const quiet = (el) => { el.setAttribute("data-1p-ignore", ""); el.setAttribute("data-lpignore", "true"); el.setAttribute("data-bwignore", ""); };
  const fields = "input, textarea, select, form";
  // Except while a passkey request is handed to 1Password (passkey.js): its prompt has to stay until the person answers.
  const handedOn = () => { for (const w of [window, top]) try { if (w.__agentBridgeHandoff > 0) return true; } catch (e) {} return false; };
  const foreign = (n) => !handedOn() && (/^COM-1PASSWORD|^ONEPASSWORD-|^LASTPASS-|^BITWARDEN-/i.test(n.tagName) ||
    (n.tagName === "IFRAME" && /^chrome-extension:/i.test(n.getAttribute("src") || "")));
  const visit = (n) => {
    if (n.nodeType !== 1) return;
    if (foreign(n)) { n.remove(); return; }
    if (/^(INPUT|TEXTAREA|SELECT|FORM)$/.test(n.tagName)) quiet(n);
    if (n.querySelectorAll) {
      n.querySelectorAll(fields).forEach(quiet);
      n.querySelectorAll("*").forEach((c) => { if (foreign(c)) c.remove(); });
    }
  };
  const watch = (root) => new MutationObserver((ms) => { for (const m of ms) for (const n of m.addedNodes) visit(n); })
    .observe(root, { childList: true, subtree: true });
  watch(document);
  // the page's own shadow roots (open or closed): watch inside them too
  const attach = Element.prototype.attachShadow;
  Element.prototype.attachShadow = function (init) { const root = attach.call(this, init); try { watch(root); } catch (e) {} return root; };
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", () => visit(document.documentElement));
  else visit(document.documentElement);
})();
