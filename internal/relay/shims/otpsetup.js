// The 2-step setup key a page shows, for the relay only (never returned to the agent): otpauth:// URIs in links,
// attributes or text, then QR codes (Chrome's BarcodeDetector), then base32 keys shown as text.
(async () => {
  const uris = new Set(), keys = new Set();
  const scan = (s) => { for (const m of String(s || "").match(/otpauth:\/\/[^\s"'<>]+/gi) || []) uris.add(m); };
  for (const e of document.querySelectorAll("*")) {
    for (const at of e.attributes || []) scan(at.value);
    if (e.value) scan(e.value);
  }
  scan(document.body ? document.body.innerText : "");
  if (!uris.size && "BarcodeDetector" in window) {
    const d = new BarcodeDetector({ formats: ["qr_code"] });
    for (const el of document.querySelectorAll("img, canvas, svg")) {
      try {
        let src = el;
        if (el.tagName.toLowerCase() === "svg") {
          const img = new Image();
          img.src = "data:image/svg+xml;base64," + btoa(unescape(encodeURIComponent(new XMLSerializer().serializeToString(el))));
          await img.decode();
          src = img;
        }
        for (const c of await d.detect(src)) if (/^otpauth:/i.test(c.rawValue)) uris.add(c.rawValue);
      } catch (e) {}
    }
  }
  if (!uris.size) {
    const texts = [];
    const w = document.createTreeWalker(document.body || document.documentElement, NodeFilter.SHOW_TEXT);
    while (w.nextNode()) texts.push(w.currentNode.textContent);
    for (const e of document.querySelectorAll("input, textarea")) texts.push(e.value || "");
    for (const t of texts) {
      const k = t.replace(/[\s-]/g, "").toUpperCase();
      if (/^[A-Z2-7]{16,128}=*$/.test(k) && (/[2-7]/.test(k) || k.length >= 26)) keys.add(k);
    }
  }
  return { uris: [...uris], keys: [...keys] };
})()
