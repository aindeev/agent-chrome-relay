// Chrome Relay's driver: one call reads every control an agent could act on, and keeps the live nodes so a later
// action names an index, never a selector. Adapted from browser-use/jev-ultrafast's snapshot.js (MIT,
// Copyright (c) 2026 Browser Use), extended to open shadow roots, controls outside the viewport, password fields
// (never their values) and checks before and after acting.
(() => {
  const D = window.__crDrive ||= { ids: new WeakMap(), nodes: new Map(), next: 1 };
  const identity = e => {
    if (!D.ids.has(e)) D.ids.set(e, D.next++);
    const id = D.ids.get(e); D.nodes.set(id, e); return id;
  };
  for (const [id, e] of D.nodes) if (!e.isConnected) D.nodes.delete(id);
  const roles = ['button', 'link', 'checkbox', 'radio', 'switch', 'tab', 'menuitem', 'menuitemcheckbox', 'menuitemradio',
    'option', 'gridcell', 'combobox', 'textbox', 'searchbox', 'spinbutton', 'treeitem'];
  const selector = 'a[href],button,input,textarea,select,summary,[contenteditable="true"],[contenteditable=""],' +
    roles.map(r => '[role="' + r + '"]').join(',');
  const skip = e => ['file', 'hidden'].includes(e.type);
  // ancestors across shadow boundaries
  const up = e => e.parentElement || (e.parentNode instanceof ShadowRoot ? e.parentNode.host : null);
  const closest = (e, sel) => { for (let x = e; x; x = up(x)) if (x.matches?.(sel)) return x; return null; };
  const visible = e => !closest(e, '[aria-hidden="true"],[inert]') && e.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true });
  const byId = (e, id) => e.getRootNode().getElementById?.(id) || document.getElementById(id);
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
  const role = e => {
    const explicit = e.getAttribute('role');
    if (roles.includes(explicit)) return explicit;
    if (e.tagName === 'BUTTON' || e.tagName === 'SUMMARY') return 'button';
    if (e.tagName === 'A') return 'link';
    if (e.tagName === 'SELECT') return 'combobox';
    if (e.tagName === 'TEXTAREA' || e.isContentEditable) return 'textbox';
    if (e.tagName === 'INPUT') {
      if (['checkbox', 'radio'].includes(e.type)) return e.type;
      if (['button', 'submit', 'reset', 'image'].includes(e.type)) return 'button';
      if (e.type === 'search') return 'searchbox';
      if (e.type === 'number') return 'spinbutton';
      if (e.type === 'password') return 'password';
      return 'textbox';
    }
    return null;
  };
  const editable = (e, r) => !e.readOnly && e.getAttribute('aria-readonly') !== 'true' &&
    (['textbox', 'searchbox', 'spinbutton', 'password'].includes(r) || (r === 'combobox' && ['INPUT', 'TEXTAREA'].includes(e.tagName)));
  const valueOf = e => e.type === 'password' ? '' : 'value' in e && e.tagName !== 'BUTTON' ? String(e.value) :
    e.isContentEditable || e.getAttribute('role') === 'combobox' ? (e.innerText || '').trim().slice(0, 200) : '';
  const labelOf = (e, r) => name(e) || (r === 'password' ? 'Password' : r);
  D.describe = e => { const r = role(e); return { role: r, label: labelOf(e, r) }; };
  // every candidate, open shadow roots included, document order
  const found = [];
  const walk = (root, budget) => {
    for (const e of root.querySelectorAll(selector)) found.push(e);
    for (const h of root.querySelectorAll('*')) if (h.shadowRoot && budget.n-- > 0) walk(h.shadowRoot, budget);
  };
  walk(document, { n: 2000 });
  const vh = innerHeight, vw = innerWidth, elements = [];
  for (const e of found) {
    if (skip(e) || !visible(e) || e.matches(':disabled') || closest(e, '[aria-disabled="true"]')) continue;
    const r = e.getBoundingClientRect(), rname = role(e);
    if (!rname || r.width <= 0 || r.height <= 0) continue;
    if (rname === 'gridcell' && e.querySelector('button,[role="button"],a[href]')) continue;
    const el = { node: identity(e), role: rname, label: labelOf(e, rname),
      onscreen: r.bottom > 0 && r.top < vh && r.right > 0 && r.left < vw, y: Math.round(r.top) };
    for (const key of ['checked', 'selected', 'expanded']) {
      const v = e.getAttribute('aria-' + key);
      if (v !== null) el[key] = v;
    }
    if (['checkbox', 'radio'].includes(e.type)) el.checked = String(e.checked);
    if (e.tagName === 'SELECT') {
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
  const omitted = Math.max(0, elements.length - 300);
  elements.splice(300);
  const words = [], walker = document.createTreeWalker(document.body || document.documentElement, NodeFilter.SHOW_TEXT);
  const range = document.createRange(); let n, length = 0;
  while ((n = walker.nextNode()) && length < 4000) {
    const v = n.textContent.trim(), parent = n.parentElement;
    if (!v || !parent || parent.closest('script,style,noscript,template') || !visible(parent)) continue;
    range.selectNodeContents(n); const r = range.getBoundingClientRect();
    if (r.width > 0 && r.height > 0 && r.bottom > 0 && r.top < vh) { words.push(v); length += v.length + 1; }
  }
  // text inside open shadow roots (the walker above doesn't enter them)
  const shadowText = root => { for (const h of root.querySelectorAll('*')) if (h.shadowRoot && length < 4000) {
    for (const c of h.shadowRoot.children) if (!['STYLE', 'SCRIPT'].includes(c.tagName)) {
      const v = (c.innerText || '').trim(); if (v) { words.push(v); length += v.length + 1; }
    }
    shadowText(h.shadowRoot);
  } };
  shadowText(document);
  const dialog = [...document.querySelectorAll('dialog[open],[role="dialog"],[role="alertdialog"]')].filter(visible)
    .map(d => name(d) || (d.innerText || '').trim().slice(0, 120))[0] || '';
  D.key = location.href + '|' + performance.timeOrigin;
  return { url: location.href, title: document.title, text: words.join('\n').slice(0, 4000), dialog, key: D.key,
    ready: document.readyState, scroll: { y: Math.round(scrollY), height: document.documentElement.scrollHeight, view: vh },
    elements, omitted };
})()
