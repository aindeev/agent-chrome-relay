// focused.js (internal/relay/shims): what has the focus, as the relay asks before typing a 1Password secret.
// It follows the focus into frames of the page's own origin (their contentDocument is reachable) and stops at
// another site's frame (contentDocument is null there).
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

const src = readFileSync(new URL("../internal/relay/shims/focused.js", import.meta.url), "utf8");

function input(type, doc) {
  return { tagName: "INPUT", type, maxLength: -1, name: "", id: "", placeholder: "", autocomplete: "", labels: [],
    ownerDocument: doc, getAttribute: () => null, getClientRects: () => [{}], parentElement: null };
}
function doc() {
  const d = { body: { tagName: "BODY" }, hasFocus: () => true };
  d.activeElement = d.body;
  return d;
}
function frame(content) {
  return { tagName: "IFRAME", contentDocument: content };
}
function focused(document) {
  return new Function("document", "window", "return " + src)(document, {});
}

test("a field of the page itself", () => {
  const d = doc();
  d.activeElement = input("password", d);
  const f = focused(d);
  assert.equal(f.type, "password");
  assert.equal(f.inner, false);
});

test("a field in a frame of the page's own origin is the field, marked inner", () => {
  const top = doc(), inner = doc();
  inner.activeElement = input("password", inner);
  top.activeElement = frame(inner);
  const f = focused(top);
  assert.equal(f.frame, undefined);
  assert.equal(f.type, "password");
  assert.equal(f.inner, true);
});

test("another site's frame stays a frame", () => {
  const top = doc();
  top.activeElement = frame(null);
  assert.deepEqual(focused(top), { frame: true, focus: true });
});

test("a same-origin frame holding another site's frame stays a frame", () => {
  const top = doc(), inner = doc();
  inner.activeElement = frame(null);
  top.activeElement = frame(inner);
  assert.deepEqual(focused(top), { frame: true, focus: true });
});

test("a same-origin frame with nothing focused in it stays a frame", () => {
  const top = doc(), inner = doc();
  top.activeElement = frame(inner);
  assert.deepEqual(focused(top), { frame: true, focus: true });
});
