(() => { let el = document.activeElement; while (el && el.shadowRoot && el.shadowRoot.activeElement) el = el.shadowRoot.activeElement;
  if (!el || el === document.body) return null; if (/^(IFRAME|FRAME)$/.test(el.tagName)) return { frame: true };
  if (!(el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.isContentEditable)) return null;
  return { tag: el.tagName, type: el.tagName === "INPUT" ? String(el.type || "text").toLowerCase() : "" }; })()
