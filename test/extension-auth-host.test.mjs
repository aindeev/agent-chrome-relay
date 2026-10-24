import { test } from 'node:test';
import assert from 'node:assert/strict';
import { extensionFixture } from './helpers/extension.mjs';

// siteAccounts is keyed by hostname, without a port. A hop that returns the port, or that
// gives up on the first relative redirect, clicks the profile email instead of the site's account.
test('a Google hop names the site it returns to, without a port', async () => {
  const f = await extensionFixture();
  assert.equal(
    f.hostOfAuth('https://accounts.google.com/o/oauth2/auth?redirect_uri=https%3A%2F%2Fapp.example.com%3A8443%2Fcb'),
    'app.example.com',
  );
  assert.equal(
    f.hostOfAuth('https://accounts.google.com/AccountChooser?redirect_uri=%2Fcallback&continue=https%3A%2F%2Fshop.example.com%2Fhome'),
    'shop.example.com',
  );
  assert.equal(f.hostOfAuth('https://accounts.google.com/ServiceLogin'), '');
});
