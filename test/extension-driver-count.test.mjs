import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const act = readFileSync(new URL('../internal/relay/shims/driveact.js', import.meta.url), 'utf8');

function root(nodes) {
  return {
    body: {},
    querySelectorAll(selector) {
      if (selector === '[') throw new SyntaxError('invalid selector');
      return selector === '*' ? nodes : nodes.filter(node => node.classes?.includes(selector));
    },
    querySelector(selector) { return this.querySelectorAll(selector)[0] || null; },
  };
}
function fixture() {
  const field = (classes) => ({ tagName: 'BUTTON', classes, isConnected: true });
  const nested = root([field(['.item', '.shadow-only'])]);
  const shadow = root([field(['.item', '.shadow-only']), { tagName: 'DIV', shadowRoot: nested }]);
  const frame = root([field(['.item', '.frame-only']), { tagName: 'DIV', shadowRoot: root([field(['.item'])]) }]);
  const document = root([
    field(['.item', '.top-only']), { tagName: 'DIV', shadowRoot: shadow },
    { tagName: 'IFRAME', contentDocument: frame }, { tagName: 'IFRAME', contentDocument: null },
  ]);
  const window = { __crDrive: { mo: {}, nodes: new Map(), identity() {} } };
  return (selector) => new Function('document', 'window', 'return (' + act + ')("query", { what: "count", selector: ' + JSON.stringify(selector) + ' })')(document, window);
}

test('count includes matching controls in open shadow roots and same-origin frames', () => {
  const count = fixture();
  assert.deepEqual(count('.item'), { value: 5 });
  assert.deepEqual(count('.shadow-only'), { value: 2 });
  assert.deepEqual(count('.frame-only'), { value: 1 });
});

test('count preserves document-only, missing-selector and invalid-selector behavior', () => {
  const count = fixture();
  assert.deepEqual(count('.top-only'), { value: 1 });
  assert.deepEqual(count('.missing'), { value: 0 });
  assert.deepEqual(count('['), { error: 'not a valid selector: [' });
});
