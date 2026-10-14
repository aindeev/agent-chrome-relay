// Chrome Relay's driver: one call reads every control an agent could act on, and keeps the live nodes so a later
// action names an index, never a selector. Adapted from browser-use/jev-ultrafast's snapshot.js (MIT,
// Copyright (c) 2026 Browser Use), extended to open shadow roots, same-origin frames, controls outside the
// viewport, clickable elements without a role, password and file fields (never their values), and checks before
// and after acting.
(() => {
  // refs never repeat in a session: each new document numbers on from where the last left off (the relay passes
  // __crDriveStart), so a ref from a page that's gone can't name a control on the next one
  const D = window.__crDrive ||= { ids: new WeakMap(), nodes: new Map(), next: 1 };
  if (window.__crDriveStart > D.next) D.next = window.__crDriveStart;
  const identity = e => {
    if (!D.ids.has(e)) D.ids.set(e, D.next++);
    const id = D.ids.get(e); D.nodes.set(id, e); return id;
  };
  D.identity = identity;
  for (const [id, e] of D.nodes) if (!e.isConnected) D.nodes.delete(id);
  const roles = ['button', 'link', 'checkbox', 'radio', 'switch', 'tab', 'menuitem', 'menuitemcheckbox', 'menuitemradio',
    'option', 'gridcell', 'combobox', 'textbox', 'searchbox', 'spinbutton', 'treeitem', 'slider'];
  const selector = 'a[href],button,input,textarea,select,summary,[contenteditable="true"],[contenteditable=""],' +
    roles.map(r => '[role="' + r + '"]').join(',');
  // ancestors across shadow roots and same-origin frames (nodeType 11: a shadow root, of any frame's window)
  const up = e => e.parentElement || (e.parentNode && e.parentNode.nodeType === 11 && e.parentNode.host) ||
    (e.ownerDocument !== document && e.ownerDocument.defaultView && e.ownerDocument.defaultView.frameElement) || null;
  D.up = up;
  D.selector = selector;
  const closest = (e, sel) => { for (let x = e; x; x = up(x)) if (x.matches?.(sel)) return x; return null; };
  // transparent counts as there (like a heading's permalink that shows on hover; a click lands on it), unless it
  // also ignores the pointer (a closed menu fading out)
  const visible = e => {
    if (closest(e, '[aria-hidden="true"],[inert]')) return false;
    if (!e.checkVisibility({ checkOpacity: true }) && getComputedStyle(e).pointerEvents === 'none') return false;
    for (let x = e; x; ) { // the element, and every frame it sits in
      if (!x.checkVisibility({ checkVisibilityCSS: true })) return false;
      const w = x.ownerDocument.defaultView;
      x = w && w !== window ? w.frameElement : null;
    }
    return true;
  };
  // where the element is in the top page's viewport
  // what to point at for a control: itself, or, for a checkbox or radio the page hides behind its styled label,
  // that label
  const box = e => {
    if (!['checkbox', 'radio'].includes(e.type) || !e.labels) return e;
    const r = e.getBoundingClientRect();
    if (visible(e) && r.width >= 2 && r.height >= 2) return e;
    return [...e.labels].find(l => visible(l) && l.getBoundingClientRect().width > 0) || e;
  };
  D.box = box;
  const rect = e => {
    e = box(e);
    let r = e.getBoundingClientRect();
    if (!r.width || !r.height) { // a link with no box of its own around a positioned image: what's inside it
      let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity, n = 0;
      for (const c of e.querySelectorAll('*')) {
        if (++n > 50) break;
        const cr = c.getBoundingClientRect();
        if (!cr.width || !cr.height) continue;
        x0 = Math.min(x0, cr.x); y0 = Math.min(y0, cr.y); x1 = Math.max(x1, cr.right); y1 = Math.max(y1, cr.bottom);
      }
      if (x1 > x0) r = { x: x0, y: y0, width: x1 - x0, height: y1 - y0 };
    }
    let x = r.x, y = r.y;
    for (let w = e.ownerDocument.defaultView; w && w !== window; ) {
      const f = w.frameElement; if (!f) break;
      const fr = f.getBoundingClientRect();
      x += fr.x + f.clientLeft; y += fr.y + f.clientTop;
      w = f.ownerDocument.defaultView;
    }
    return { x, y, width: r.width, height: r.height };
  };
  D.rect = rect;
  const byId = (e, id) => e.getRootNode().getElementById?.(id) || e.ownerDocument.getElementById(id);
  const name = (e, seen = new Set()) => {
    if (!e || seen.has(e)) return '';
    seen.add(e);
    const referenced = (e.getAttribute('aria-labelledby') || '').split(/\s+/).filter(Boolean)
      .map(id => name(byId(e, id), seen)).filter(Boolean).join(' ');
    const own = referenced || e.getAttribute('aria-label') ||
      [...(e.labels || [])].map(l => name(l, seen)).filter(Boolean).join(' ') ||
      (['button', 'submit', 'reset'].includes(e.type) ? e.value : '') || e.getAttribute('alt') ||
      (e.tagName === 'INPUT' ? '' : [...(e.shadowRoot ? [...e.shadowRoot.childNodes, ...e.childNodes] : e.childNodes)]
        .map(n => n.nodeType === 3 ? n.textContent : n.nodeType === 1 && n.getAttribute('aria-hidden') !== 'true' &&
          !['SCRIPT', 'STYLE'].includes(n.tagName) ? name(n, seen) : '').join(' ')) ||
      e.getAttribute('title') || e.getAttribute('placeholder') || '';
    return own.replace(/\s+/g, ' ').trim().slice(0, 200);
  };
  D.generic ||= new WeakSet();
  const role = e => {
    const explicit = e.getAttribute('role');
    if (roles.includes(explicit)) return explicit;
    if (e.tagName === 'BUTTON' || e.tagName === 'SUMMARY') return 'button';
    if (e.tagName === 'A' && e.hasAttribute('href')) return 'link';
    if (e.tagName === 'SELECT') return 'combobox';
    if (e.tagName === 'TEXTAREA' || e.isContentEditable) return 'textbox';
    if (e.tagName === 'INPUT') {
      if (['checkbox', 'radio'].includes(e.type)) return e.type;
      if (['button', 'submit', 'reset', 'image'].includes(e.type)) return 'button';
      if (e.type === 'search') return 'searchbox';
      if (e.type === 'number') return 'spinbutton';
      if (e.type === 'range') return 'slider';
      if (e.type === 'password') return 'password';
      if (e.type === 'file') return 'file';
      return 'textbox';
    }
    return D.generic.has(e) ? 'clickable' : null;
  };
  const editable = (e, r) => !e.readOnly && e.getAttribute('aria-readonly') !== 'true' &&
    (['textbox', 'searchbox', 'spinbutton', 'password'].includes(r) || (r === 'combobox' && ['INPUT', 'TEXTAREA'].includes(e.tagName)));
  const valueOf = e => e.type === 'password' || e.type === 'file' ? '' : ['INPUT', 'TEXTAREA', 'SELECT'].includes(e.tagName) ? String(e.value) :
    e.isContentEditable || e.getAttribute('role') === 'combobox' ? (e.innerText || '').trim().slice(0, 200) : '';
  const labelOf = (e, r) => name(e) || (r === 'password' ? 'Password' : r === 'file' ? 'Choose file' : r);
  D.describe = e => { const r = role(e); return { role: r, label: labelOf(e, r) }; };
  // every candidate, open shadow roots and same-origin frames included, document order
  const found = [], roots = [document], docs = [document];
  const frameDoc = f => { try { return f.contentDocument && f.contentDocument.body ? f.contentDocument : null; } catch { return null; } };
  const walk = (root, budget) => {
    for (const e of root.querySelectorAll(selector)) found.push(e);
    for (const h of root.querySelectorAll('*')) {
      if (budget.n <= 0) return;
      if (h.shadowRoot) { budget.n--; roots.push(h.shadowRoot); walk(h.shadowRoot, budget); }
      else if (h.tagName === 'IFRAME' || h.tagName === 'FRAME') {
        const d = frameDoc(h);
        if (d) { budget.n--; roots.push(d); docs.push(d); walk(d, budget); }
      }
    }
  };
  walk(document, { n: 2000 });
  // clickable things that have no role: an onclick, a tabindex, or a pointer cursor of their own (not inherited),
  // that neither sit inside a control nor wrap one. Many apps build buttons out of plain divs.
  const candidates = new Set(found), generic = [];
  const budget = { n: 8000 };
  for (const root of roots) {
    for (const e of root.querySelectorAll('div,span,li,td,th,tr,img,svg,p,h1,h2,h3,h4,h5,h6,i,b,em,strong,section,article,label,[onclick],[tabindex]')) {
      if (budget.n-- <= 0 || generic.length >= 150) break;
      if (candidates.has(e) || (e.tagName === 'LABEL' && (e.control || e.htmlFor))) continue;
      const tab = e.getAttribute('tabindex');
      let hit = e.hasAttribute('onclick') || (tab !== null && tab !== '-1');
      if (!hit) {
        const view = e.ownerDocument.defaultView, p = up(e);
        hit = view.getComputedStyle(e).cursor === 'pointer' &&
          !(p && p.nodeType === 1 && p.ownerDocument.defaultView.getComputedStyle(p).cursor === 'pointer');
      }
      if (!hit) continue;
      let inside = false;
      for (let x = up(e), i = 0; x && i < 8; x = up(x), i++) if (candidates.has(x) || D.generic.has(x) && generic.includes(x)) { inside = true; break; }
      if (inside || e.querySelector(selector) || !name(e)) continue;
      D.generic.add(e); generic.push(e);
    }
  }
  // one control, one entry: a link inside a suggestion with the same label (a common autocomplete markup) is the
  // same choice, and listing it twice splits a decision model's confidence between the two
  const same = new Map();
  const dup = e => {
    if (!e.matches('a[href],[role="link"],[role="button"],button')) return false;
    for (let x = up(e), i = 0; x && i < 6; x = up(x), i++) if (candidates.has(x) && !x.matches('input,textarea,select,form')) {
      const outer = same.get(x) ?? same.set(x, name(x)).get(x);
      return outer === name(e);
    }
    return false;
  };
  const vh = innerHeight, vw = innerWidth, elements = [];
  for (const e of found.concat(generic)) {
    if (dup(e)) continue;
    if (e.type === 'hidden' || e.matches(':disabled') || closest(e, '[aria-disabled="true"]')) continue;
    const rname = role(e);
    if (!rname) continue;
    const r = rect(e);
    // a file input is often hidden behind a styled button: keep it, files are set on it without a click
    if (rname !== 'file' && (!visible(box(e)) || r.width <= 0 || r.height <= 0)) continue;
    if (rname === 'gridcell' && e.querySelector('button,[role="button"],a[href]')) continue;
    const el = { node: identity(e), role: rname, label: labelOf(e, rname),
      onscreen: r.y + r.height > 0 && r.y < vh && r.x + r.width > 0 && r.x < vw, y: Math.round(r.y) };
    for (const key of ['checked', 'selected', 'expanded']) {
      const v = e.getAttribute('aria-' + key);
      if (v !== null) el[key] = v;
    }
    if (['checkbox', 'radio'].includes(e.type)) el.checked = String(e.checked);
    if (e.ownerDocument !== document) el.frame = true;
    if (rname === 'file') {
      el.ops = ['upload'];
      el.value = [...(e.files || [])].map(f => f.name).join(', ');
    } else if (e.tagName === 'SELECT') {
      el.ops = ['select'];
      el.value = [...e.selectedOptions].map(o => o.label).join(', ');
      el.options = [...e.options].filter(o => !o.disabled && !o.closest('optgroup[disabled]')).slice(0, 60).map(o => o.label.trim() || o.value);
    } else {
      const ed = editable(e, rname);
      el.ops = ed ? ['fill', 'click'] : ['click'];
      if (rname === 'password') el.filled = !!e.value; else el.value = valueOf(e);
      if (ed && e.required) el.required = true;
    }
    elements.push(el);
  }
  // what's on screen first, then nearest below
  elements.sort((a, b) => (b.onscreen - a.onscreen) || (a.onscreen ? 0 : Math.abs(a.y) - Math.abs(b.y)));
  const limit = window.__crDriveLimit || 300;
  const omitted = Math.max(0, elements.length - limit);
  elements.splice(limit);
  const words = []; let length = 0;
  for (const d of docs) {
    const top = d.body || d.documentElement;
    if (!top) continue; // a document between two pages
    const walker = d.createTreeWalker(top, NodeFilter.SHOW_TEXT);
    const range = d.createRange(); let n;
    while ((n = walker.nextNode()) && length < 4000) {
      const v = n.textContent.trim(), parent = n.parentElement;
      if (!v || !parent || parent.closest('script,style,noscript,template') || !visible(parent)) continue;
      range.selectNodeContents(n); const r = range.getBoundingClientRect();
      if (r.width > 0 && r.height > 0 && (d !== document || (r.bottom > 0 && r.top < vh))) { words.push(v); length += v.length + 1; }
    }
  }
  // text inside open shadow roots (the walker above doesn't enter them)
  for (const root of roots) if (root.nodeType === 11 && length < 4000) {
    for (const c of root.children) if (!['STYLE', 'SCRIPT'].includes(c.tagName)) {
      const v = (c.innerText || '').trim(); if (v) { words.push(v); length += v.length + 1; }
    }
  }
  const dialog = [...document.querySelectorAll('dialog[open],[role="dialog"],[role="alertdialog"]')].filter(visible)
    .map(d => name(d) || (d.innerText || '').trim().slice(0, 120))[0] || '';
  // frames this reader cannot enter (another site's): the agent should know they are there
  const otherFrames = [...document.querySelectorAll('iframe,frame')]
    .filter(f => !frameDoc(f) && f.src && visible(f) && f.getBoundingClientRect().width > 40 && f.getBoundingClientRect().height > 40)
    .map(f => { try { return new URL(f.src, location.href).host; } catch { return ''; } }).filter(Boolean).slice(0, 5);
  D.docs = docs; D.roots = roots;
  D.key = location.href + '|' + performance.timeOrigin;
  return { url: location.href, title: document.title, text: words.join('\n').slice(0, 4000), dialog, key: D.key,
    ready: document.readyState, scroll: { y: Math.round(scrollY), height: document.documentElement.scrollHeight, view: vh },
    elements, omitted, otherFrames, next: D.next };
})()
