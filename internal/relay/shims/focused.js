(() => { let el = document.activeElement, inner = false;
  // into shadow roots, and into frames of the page's own origin (Apple's sign-in form): the page's script can reach
  // into those itself, so they are the page. Another origin's frame has no contentDocument here, and stays a frame.
  for (;;) {
    while (el && el.shadowRoot && el.shadowRoot.activeElement) el = el.shadowRoot.activeElement;
    let d = null;
    if (el && /^(IFRAME|FRAME)$/.test(el.tagName)) try { d = el.contentDocument; } catch (e) {}
    if (!d || !d.activeElement || d.activeElement === d.body) break;
    el = d.activeElement; inner = true;
  }
  const doc = (el && el.ownerDocument) || document;
  if (!el || el === doc.body) return null; if (/^(IFRAME|FRAME)$/.test(el.tagName)) return { frame: true, focus: document.hasFocus() };
  if (!(el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.isContentEditable)) return null;
  // a code split over one-character boxes (six inputs for a six-digit code): the boxes from this one on, kept for focusing in turn
  const one = (e) => e.tagName === "INPUT" && e.maxLength === 1 && !/^(checkbox|radio|hidden|submit|button)$/i.test(e.type) && e.getClientRects().length > 0;
  let boxes = 0;
  if (one(el)) for (let c = el.parentElement, i = 0; c && i < 5; c = c.parentElement, i++) {
    const all = [...c.querySelectorAll("input")].filter(one);
    if (all.length >= 4) { const g = all.slice(all.indexOf(el)); window[Symbol.for("chrome-relay.boxes")] = g; boxes = g.length; break; }
  }
  // what kind of card field it is, if any: its autocomplete token, else its name, id, placeholder and label
  const hint = [el.name, el.id, el.placeholder, el.getAttribute("aria-label"), ...(el.labels ? [...el.labels].map(l => l.innerText) : [])].join(" ").toLowerCase();
  return { tag: el.tagName, type: el.tagName === "INPUT" ? String(el.type || "text").toLowerCase() : "", boxes,
    ac: String(el.autocomplete || "").toLowerCase(), hint: hint.slice(0, 300), max: el.maxLength > 0 ? el.maxLength : 0, focus: document.hasFocus(), inner }; })()
