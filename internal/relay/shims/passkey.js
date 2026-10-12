(() => {
  if (window.top !== window || window.__agentBridgeDone || !window.CredentialsContainer || !navigator.credentials) return;
  const C = navigator.credentials, P = CredentialsContainer.prototype, native = { get: P.get, create: P.create };
  const delegate = {}, handedOn = new WeakSet();   // another passkey provider's wrapper; requests already handed to it
  const keep = (k, d) => { if (k !== "get" && k !== "create" || !d) return false; delegate[k] = "value" in d ? () => d.value : d.get ? () => d.get.call(C) : null; return true; };
  const dp = Object.defineProperty, rdp = Reflect.defineProperty, dps = Object.defineProperties;
  Object.defineProperty = function defineProperty(o, k, d) { return o === C && keep(k, d) ? o : dp.apply(this, arguments); };
  Reflect.defineProperty = function defineProperty(o, k, d) { return o === C && keep(k, d) ? true : rdp.apply(this, arguments); };
  Object.defineProperties = function defineProperties(o, ds) {
    if (o !== C || !ds) return dps.apply(this, arguments);
    const rest = {}; for (const k of Object.keys(ds)) if (!keep(k, ds[k])) rest[k] = ds[k]; return dps.call(this, o, rest);
  };
  const send = (m) => { try { __agentBridgeOpen(JSON.stringify(m)); } catch (e) {} };
  const b64 = (u8) => { let s = ""; for (const c of u8) s += String.fromCharCode(c); return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, ""); };
  const buf = (s) => Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/")), (c) => c.charCodeAt(0)).buffer;
  const enc = (v) => v instanceof ArrayBuffer ? { $b: b64(new Uint8Array(v)) } : ArrayBuffer.isView(v) ? { $b: b64(new Uint8Array(v.buffer, v.byteOffset, v.byteLength)) }
    : Array.isArray(v) ? v.map(enc) : v && typeof v === "object" ? Object.fromEntries(Object.entries(v).map(([k, x]) => [k, enc(x)])) : v;
  // writable, like the real ones: pages may assign their own toJSON onto a credential (GitHub does)
  const own = (o) => Object.fromEntries(Object.entries(o).map(([k, value]) => [k, { value, enumerable: true, writable: true, configurable: true }]));
  const credential = (r, kind) => {
    const x = r.response, made = kind === "create";
    const response = Object.create((made ? AuthenticatorAttestationResponse : AuthenticatorAssertionResponse).prototype, own(made
      ? { clientDataJSON: buf(x.clientDataJSON), attestationObject: buf(x.attestationObject), getTransports: () => x.transports.slice(),
          getAuthenticatorData: () => buf(x.authenticatorData), getPublicKey: () => buf(x.publicKey), getPublicKeyAlgorithm: () => x.publicKeyAlgorithm }
      : { clientDataJSON: buf(x.clientDataJSON), authenticatorData: buf(x.authenticatorData), signature: buf(x.signature), userHandle: x.userHandle == null ? null : buf(x.userHandle) }));
    return Object.create(PublicKeyCredential.prototype, own({ id: r.id, rawId: buf(r.id), type: r.type, authenticatorAttachment: r.authenticatorAttachment, response,
      getClientExtensionResults: () => ({ ...r.clientExtensionResults }),
      toJSON: () => ({ id: r.id, rawId: r.id, type: r.type, authenticatorAttachment: r.authenticatorAttachment, clientExtensionResults: r.clientExtensionResults, response: x }) }));
  };
  const waiting = new Map(); let n = 0;
  const aborted = (signal) => signal.reason !== undefined ? signal.reason : new DOMException("The operation was aborted.", "AbortError");
  const ask = (kind, options) => new Promise((resolve, reject) => {
    const signal = options.signal; if (signal && signal.aborted) return reject(aborted(signal));
    const id = ++n; waiting.set(id, { resolve, reject, kind, options });
    if (signal) signal.addEventListener("abort", () => { if (waiting.delete(id)) reject(aborted(signal)); }, { once: true });
    send({ op: "webauthn", id, kind, mediation: String(options.mediation || ""), publicKey: enc(options.publicKey) });
  });
  Object.defineProperty(window, "__agentBridgeDone", { value: (id, r) => {
    const w = waiting.get(id); if (!w) return; waiting.delete(id);
    if (r.handoff) {   // not the relay's: the other provider (1Password) asks the person
      const f = delegate[w.kind] && delegate[w.kind]();
      if (typeof f !== "function") { const why = "no passkey for this site in the shared vault, and no 1Password in this browser"; console.warn("Chrome Relay passkeys:", why); return w.reject(new DOMException(why, "NotAllowedError")); }
      handedOn.add(w.options);
      // its prompt is a frame of its own in this page: quiet.js leaves those alone until the person has answered
      const held = (d) => { for (const win of [window, top]) try { win.__agentBridgeHandoff = Math.max(0, (win.__agentBridgeHandoff || 0) + d); } catch (e) {} };
      held(1); const p = Promise.resolve().then(() => f.call(C, w.options)); p.then(() => held(-1), () => held(-1));
      return w.resolve(p);
    }
    if (r.error) { console.warn("Chrome Relay passkeys:", r.message); w.reject(new DOMException(r.message, r.error)); } else w.resolve(credential(r.credential, w.kind));
  } });
  const mine = {};
  for (const k of ["get", "create"]) {
    mine[k] = function (options) { return options && options.publicKey && !handedOn.has(options) ? ask(k, options) : native[k].apply(C, arguments); };
    dp(C, k, { enumerable: true, configurable: false, get: () => mine[k], set: (f) => { delegate[k] = () => f; } });
  }
  try { dp(navigator, "credentials", { value: C, configurable: false, writable: false }); } catch (e) {}
})();
