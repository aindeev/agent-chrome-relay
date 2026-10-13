(() => { let el = document.activeElement; while (el && el.shadowRoot && el.shadowRoot.activeElement) el = el.shadowRoot.activeElement;
  if (!el || el === document.body) return null; if (/^(IFRAME|FRAME)$/.test(el.tagName)) return { frame: true };
  if (!(el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.isContentEditable)) return null;
  // a code split over one-character boxes (six inputs for a six-digit code): the boxes from this one on, kept for focusing in turn
  const one = (e) => e.tagName === "INPUT" && e.maxLength === 1 && !/^(checkbox|radio|hidden|submit|button)$/i.test(e.type) && e.getClientRects().length > 0;
  let boxes = 0;
  if (one(el)) for (let c = el.parentElement, i = 0; c && i < 5; c = c.parentElement, i++) {
    const all = [...c.querySelectorAll("input")].filter(one);
    if (all.length >= 4) { const g = all.slice(all.indexOf(el)); window[Symbol.for("chrome-relay.boxes")] = g; boxes = g.length; break; }
  }
  return { tag: el.tagName, type: el.tagName === "INPUT" ? String(el.type || "text").toLowerCase() : "", boxes }; })()
