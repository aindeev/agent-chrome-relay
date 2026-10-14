// Chrome Relay's driver, in the page: before acting, check the observed node is still the one the decision was
// about and can take the input; after, wait for the page to settle and read back what changed. Called as
// (__crDriveAct)(phase, request); __crDrive comes from driver.js.
(phase, q) => {
  const frameDoc = f => { try { return f.contentDocument && f.contentDocument.body ? f.contentDocument : null; } catch { return null; } };
  // the page's text, open shadow roots and same-origin frames included (innerText doesn't enter them); never what
  // fields hold, so a value just typed can't satisfy --until text:<that value>
  const deepText = () => {
    const parts = [];
    const visit = root => {
      for (const h of root.querySelectorAll('*')) {
        if (h.shadowRoot) {
          for (const c of h.shadowRoot.children) if (!['STYLE', 'SCRIPT'].includes(c.tagName)) parts.push(c.innerText ?? c.textContent ?? '');
          visit(h.shadowRoot);
        } else if (h.tagName === 'IFRAME' || h.tagName === 'FRAME') {
          const d = frameDoc(h);
          if (d) { parts.push(d.body.innerText || ''); visit(d); }
        }
      }
    };
    parts.push(document.body?.innerText || '');
    visit(document);
    return parts.join('\n');
  };
  // the first match in the page, its open shadow roots and same-origin frames
  function deepQuery(sel) {
    const search = root => {
      const hit = root.querySelector(sel);
      if (hit) return hit;
      for (const h of root.querySelectorAll('*')) {
        const inner = h.shadowRoot || ((h.tagName === 'IFRAME' || h.tagName === 'FRAME') && frameDoc(h));
        const found = inner && search(inner);
        if (found) return found;
      }
      return null;
    };
    return search(document);
  }
  if (phase === 'check') {
    const text = deepText();
    return q.checks.map(c => {
      try {
        if (c.kind === 'text') return text.toLowerCase().includes(c.value.toLowerCase());
        if (c.kind === 'no-text') return !text.toLowerCase().includes(c.value.toLowerCase());
        if (c.kind === 'url') return location.href.includes(c.value);
        if (c.kind === 'title') return document.title.toLowerCase().includes(c.value.toLowerCase());
        if (c.kind === 'selector') return !!deepQuery(c.value);
      } catch { return false; }
      return false;
    });
  }
  const D = window.__crDrive;
  if (phase === 'text') {
    if (q.selector) {
      let el;
      try { el = deepQuery(q.selector); } catch { return { error: 'not a valid selector: ' + q.selector }; }
      if (!el) return { error: 'nothing matches ' + q.selector };
      return { text: (el.type === 'password' ? '' : ['INPUT', 'TEXTAREA', 'SELECT'].includes(el.tagName) ? el.value : el.innerText ?? el.textContent ?? '').toString().slice(0, 200000) };
    }
    if (q.node) {
      const el = D && D.nodes.get(q.node);
      if (!el?.isConnected) return { error: 'that element is gone: take a new snapshot' };
      return { text: (el.type === 'password' ? '' : ['INPUT', 'TEXTAREA', 'SELECT'].includes(el.tagName) ? el.value : el.innerText ?? el.textContent ?? '').toString().slice(0, 200000) };
    }
    return { text: deepText().slice(0, 200000) };
  }
  // D lives in this document's script world: none means the page was replaced since the snapshot. (Within one
  // document a node is still checked: connected, and, for a decision, still the same role and label.)
  if (!D) return { stale: 'the page changed: observe again' };
  // the control: a node from the snapshot, or the first match of a CSS selector (any element, not only controls)
  const resolve = (node, sel) => {
    if (!sel) return D.nodes.get(node);
    try { const el = deepQuery(sel); if (el) D.identity(el); return el; } catch { return null; }
  };
  const e = resolve(q.node, q.selector);
  const up = D.up;
  if (phase === 'query') {
    if (q.what === 'count') { try { return { value: document.querySelectorAll(q.selector).length }; } catch { return { error: 'not a valid selector: ' + q.selector }; } }
    if (!e?.isConnected) return { error: q.selector ? 'nothing matches ' + q.selector : 'that element is gone: take a new snapshot' };
    if (q.what === 'html') { // never what fields hold: a page may mirror a typed password into its value attribute
      const c = e.cloneNode(true);
      for (const f of [c, ...c.querySelectorAll('input,textarea')]) if (f.matches('input,textarea')) { f.removeAttribute('value'); f.textContent = ''; }
      return { value: c.outerHTML.slice(0, 200000) };
    }
    if (q.what === 'attr') return { value: e.getAttribute(q.name) };
    if (q.what === 'visible') return { value: e.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true }) };
    if (q.what === 'enabled') return { value: !e.matches(':disabled') && !e.closest('[aria-disabled="true"],[inert]') };
    if (q.what === 'checked') return { value: e.checked ?? e.getAttribute('aria-checked') === 'true' };
    return { error: 'unknown question' };
  }
  if (phase === 'drag') {
    const to = resolve(q.toNode, q.toSelector);
    if (!e?.isConnected || !to?.isConnected) return { refused: 'one of the two elements is gone' };
    e.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
    const a = D.rect(e), b = D.rect(to);
    const on = r => r.width && r.height && r.x + r.width / 2 >= 0 && r.y + r.height / 2 >= 0 && r.x + r.width / 2 < innerWidth && r.y + r.height / 2 < innerHeight;
    if (!on(a) || !on(b)) return { refused: 'both elements have to be on screen at once to drag between them' };
    D.before = { text: deepText() };
    return { x: a.x + a.width / 2, y: a.y + a.height / 2, tx: b.x + b.width / 2, ty: b.y + b.height / 2, node: D.identity(e) };
  }
  if (phase === 'prepare') {
    if (!e?.isConnected) return q.selector ? { refused: 'nothing matches ' + q.selector } : { stale: 'the element is gone' };
    if (q.expect) {
      const now = D.describe(e);
      if (now.role !== q.expect.role || now.label !== q.expect.label) return { stale: 'the element changed: it is now ' + now.role + ' "' + now.label + '"' };
    }
    if (e.matches(':disabled') || e.closest('[aria-disabled="true"],[inert]')) return { refused: 'the element is disabled' };
    if ((q.kind === 'fill' || q.kind === 'type') && (e.readOnly || e.getAttribute('aria-readonly') === 'true')) return { refused: 'the field is read-only' };
    D.before = { value: e.type === 'password' ? !!e.value : e.value, checked: e.checked, expanded: e.getAttribute('aria-expanded'),
      text: deepText() };
    if (q.kind === 'upload') return e.type === 'file' ? { done: true, node: D.identity(e) } : { refused: 'not a file field' };
    D.box(e).scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
    const r = D.rect(e);
    // what would get a click at (x, y): the deepest element there, through open shadow roots and same-origin frames
    const hitAt = (x, y) => {
      let doc = document, ox = 0, oy = 0, hit = null;
      for (let depth = 0; depth < 10; depth++) {
        hit = doc.elementFromPoint(x - ox, y - oy);
        while (hit?.shadowRoot) { const inner = hit.shadowRoot.elementFromPoint(x - ox, y - oy); if (!inner || inner === hit) break; hit = inner; }
        const inner = hit && (hit.tagName === 'IFRAME' || hit.tagName === 'FRAME') && frameDoc(hit);
        if (!inner) break;
        const fr = hit.getBoundingClientRect();
        ox += fr.x + hit.clientLeft; oy += fr.y + hit.clientTop; doc = inner;
      }
      return hit;
    };
    // the click must reach the element itself: not something over it, and not another control inside it (a row's
    // checkbox, a card's star button)
    const target = D.box(e);
    const reaches = hit => {
      if (!hit) return false;
      if (e.labels && [...e.labels].some(l => l.contains(hit))) return true;
      for (let h = hit; h; h = up(h)) {
        if (h === e || h === target) return true;
        // another control: over it, or inside it. Inside it with the same name (a suggestion's own link, as Wikipedia
        // marks them up) it's the same choice.
        if (h.nodeType === 1 && (h.matches(D.selector) || D.generic.has(h))) return e.contains(h) && D.describe(h).label === D.describe(e).label;
      }
      return false;
    };
    // near the left edge first (where a wide row's text or link is), then the middle, the right, the top left
    const points = [[r.x + Math.min(r.width / 2, 40), r.y + r.height / 2], [r.x + r.width / 2, r.y + r.height / 2],
      [r.x + r.width - Math.min(r.width / 2, 12), r.y + r.height / 2], [r.x + Math.min(r.width / 2, 6), r.y + Math.min(r.height / 2, 6)]]
      .filter(([x, y]) => r.width && r.height && x >= 0 && y >= 0 && x < innerWidth && y < innerHeight);
    if (!points.length) return { refused: 'the element is not on screen' };
    let x, y, first;
    for (const [px, py] of points) {
      const h = hitAt(px, py);
      if (reaches(h)) { x = px; y = py; break; }
      first ||= h;
    }
    if (x === undefined) {
      const what = first ? (first.getAttribute('aria-label') || first.innerText || first.tagName).trim().slice(0, 60) : 'nothing';
      return { refused: (first && e.contains(first) ? 'a click would land on another control inside it: ' : 'something covers it: ') + what };
    }
    if (q.kind === 'select') {
      const o = [...e.options].find(o => (o.label.trim() || o.value) === q.option && !o.disabled);
      if (!o) return { refused: 'no option "' + q.option + '"' };
      e.value = o.value;
      e.dispatchEvent(new Event('input', { bubbles: true }));
      e.dispatchEvent(new Event('change', { bubbles: true }));
      return { x, y, done: true, node: D.identity(e) };
    }
    return { x, y, node: D.identity(e) };
  }
  if (phase === 'settle') {
    // until the DOM has been quiet for 50 ms (at most 1.5 s); a combobox waits for its options. Timers, not
    // animation frames: those may not run in a background tab.
    // A click on a link to another page in this tab waits for the URL to change (at most 5 s): an app that
    // fetches the next page before swapping it in (GitHub, Turbo, Next.js) leaves the DOM quiet meanwhile.
    const link = q.kind === 'click' && e?.closest?.('a[href]');
    const from = location.href.split('#')[0];
    const leaving = !!link && (!link.target || link.target === '_self') && !link.hasAttribute('download') &&
      /^https?:/.test(link.href) && link.href.split('#')[0] !== from;
    return new Promise(resolve => {
      let quietSince = performance.now(), ticks = 0, done = false;
      const mo = new MutationObserver(() => { quietSince = performance.now(); });
      for (const d of D.docs || [document]) { try { mo.observe(d, { subtree: true, childList: true, attributes: true, characterData: true }); } catch {} }
      const finish = () => { if (!done) { done = true; mo.disconnect(); resolve(true); } };
      setTimeout(finish, leaving ? 5000 : 1500);
      const tick = () => {
        if (done) return;
        const listbox = e && e.getAttribute('role') === 'combobox' && q.kind === 'fill';
        const options = listbox && [...document.querySelectorAll('[role="option"]')].some(o => o.checkVisibility());
        if (leaving && location.href.split('#')[0] === from) quietSince = performance.now();
        if (++ticks >= 2 && performance.now() - quietSince > (listbox && !options ? 400 : 50)) finish();
        else setTimeout(tick, 20);
      };
      setTimeout(tick, 20);
    });
  }
  if (phase === 'readback') {
    if (!e?.isConnected) return { gone: true };
    const b = D.before || {};
    const value = e.type === 'password' ? !!e.value : e.value;
    return { value: e.type === 'password' ? undefined : (typeof value === 'string' ? value.slice(0, 200) : value),
      filled: e.type === 'password' ? !!e.value : undefined, checked: e.checked,
      changed: value !== b.value || e.checked !== b.checked || e.getAttribute('aria-expanded') !== b.expanded ||
        deepText() !== b.text };
  }
  return { refused: 'unknown phase' };
}
