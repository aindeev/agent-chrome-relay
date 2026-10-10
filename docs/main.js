// Chrome Relay site: small progressive enhancements. No dependencies.
(() => {
  const reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  // Top bar: solid background once the page scrolls.
  const bar = document.querySelector('.topbar');
  const onScroll = () => bar.classList.toggle('is-scrolled', window.scrollY > 40);
  onScroll();
  window.addEventListener('scroll', onScroll, { passive: true });

  // Scroll reveal with a light stagger among siblings that enter together.
  const reveals = [...document.querySelectorAll('.reveal')];
  if (reduce || !('IntersectionObserver' in window)) {
    reveals.forEach((el) => el.classList.add('is-in'));
  } else {
    const io = new IntersectionObserver((entries) => {
      let i = 0;
      entries.forEach((e) => {
        if (!e.isIntersecting) return;
        e.target.style.setProperty('--d', `${Math.min(i++, 5) * 70}ms`);
        e.target.classList.add('is-in');
        io.unobserve(e.target);
      });
    }, { rootMargin: '0px 0px -8% 0px', threshold: 0.12 });
    reveals.forEach((el) => io.observe(el));
  }

  // Copy-to-clipboard buttons.
  const copyText = async (text) => {
    try { await navigator.clipboard.writeText(text); return true; } catch {
      const ta = Object.assign(document.createElement('textarea'), { value: text });
      ta.style.cssText = 'position:fixed;opacity:0';
      document.body.append(ta); ta.select();
      const ok = document.execCommand('copy'); ta.remove(); return ok;
    }
  };
  document.querySelectorAll('[data-copy]').forEach((btn) => {
    btn.addEventListener('click', async () => {
      if (!(await copyText(btn.dataset.copy))) return;
      btn.classList.add('is-copied');
      clearTimeout(btn._t);
      btn._t = setTimeout(() => btn.classList.remove('is-copied'), 1600);
    });
  });

  // Hero demo: type the commands, mirror each step in the Chrome mock.
  const stage = document.getElementById('stage');
  const term = document.getElementById('demoTerm');
  const ghost = document.getElementById('ghost');
  const group = document.getElementById('agentsGroup');
  const count = document.getElementById('agentsCount');
  const btn = ghost && ghost.querySelector('.gp-btn');
  if (!stage || !term || reduce) return;

  const esc = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  const P = '<span class="p">$</span> ';
  const CMD1 = 'agent-browser --cdp "$(chrome-relay url you@company.com)" open https://app.example.com';
  const CMD2 = 'agent-browser snapshot -i && agent-browser click @e5 && agent-browser close';

  let visible = false;
  let wake = null;
  new IntersectionObserver(([e]) => {
    visible = e.isIntersecting;
    if (visible && wake) { wake(); wake = null; }
  }, { threshold: 0.2 }).observe(stage);
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const ready = () => (visible ? Promise.resolve() : new Promise((r) => { wake = r; }));
  const step = async (ms) => { await sleep(ms); await ready(); };

  // Reserve the finished transcript's height so the page never jumps while typing.
  const reserve = () => { term.style.minHeight = ''; term.style.minHeight = term.offsetHeight + 'px'; };
  reserve();
  let rw = window.innerWidth;
  window.addEventListener('resize', () => { if (window.innerWidth !== rw) { rw = window.innerWidth; term.style.minHeight = ''; } });

  const lines = [];
  const render = (current) => {
    term.innerHTML = lines.join('\n') + (lines.length ? '\n' : '') + (current ?? '');
  };
  const print = async (html, ms = 140) => { lines.push(html); render(''); await step(ms); };
  const type = async (cmd) => {
    for (let i = 1; i <= cmd.length; i++) {
      render(P + esc(cmd.slice(0, i)) + '<span class="cur"></span>');
      await sleep(i < 8 ? 55 : 16 + Math.random() * 22);
    }
    await step(320);
    lines.push(P + esc(cmd));
  };
  const setCount = (n) => {
    count.textContent = n;
    group.classList.toggle('is-zero', n === 0);
    group.classList.add('is-bump');
    setTimeout(() => group.classList.remove('is-bump'), 260);
  };
  const ghostState = (...on) => {
    ghost.className = 'ghost' + on.map((c) => ' ' + c).join('');
  };

  const run = async () => {
    for (;;) {
      await ready();
      lines.length = 0;
      setCount(0);
      ghostState();
      btn.classList.remove('is-pressed');
      render(P + '<span class="cur"></span>');
      await step(900);

      await type(CMD1);
      setCount(1);
      ghostState('is-on');
      await print('<span class="ok">✓</span> Example App', 90);
      await print('  <span class="dim">https://app.example.com/</span>  <span class="t">0.6 s</span>', 900);

      render(P + '<span class="cur"></span>');
      await step(500);
      await type(CMD2);
      ghostState('is-on', 'show-refs');
      await print('<span class="dim">- textbox "Search" [ref=e1]</span>', 70);
      await print('<span class="dim">- link "Projects" [ref=e3]</span>', 70);
      await print('<span class="dim">- button "New report" [ref=e5]</span>', 380);
      ghostState('is-on', 'show-refs', 'at-btn');
      await step(760);
      ghostState('is-on', 'show-refs', 'at-btn', 'clicked');
      btn.classList.add('is-pressed');
      await step(220);
      btn.classList.remove('is-pressed');
      await print('<span class="ok">✓</span> Done  <span class="t">0.15 s</span>', 700);
      ghostState();
      setCount(0);
      await print('<span class="ok">✓</span> Browser closed', 200);
      render(P + '<span class="cur"></span>');
      await step(3200);
    }
  };
  run();
})();
