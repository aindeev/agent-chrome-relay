---
name: chrome-relay
description: Use the person's own signed-in Chrome from the command line, in background tabs, through Chrome Relay. Load this BEFORE any browser step that needs their logins (internal tools, Google-SSO apps, GitHub, admin consoles, staging environments, OAuth consent screens, sign-ins with the 1Password vaults they allowed: passwords, 2-step codes, passkeys) or when they say "check it in the browser", "open it logged in", "click through", "use my Chrome". Never drive their Chrome any other way.
---

# Chrome Relay

Chrome Relay lets Claude Code, Codex, Cursor and Paseo agents drive **background tabs** in the person's
everyday Chrome, with their real logins. Your tabs open inactive in a collapsed "Agents" tab group, you only
see and control tabs you opened, and nothing on their screen moves. ~0.15s per command.

```bash
export AGENT_BROWSER_SESSION=<your-agent-name>                 # one session per agent
chrome-relay identities                                         # which profile (email) to use for what
agent-browser --cdp "$(chrome-relay url you@company.com)" open https://example.internal/
agent-browser snapshot -i            # interactive elements with refs (text, cheap)
agent-browser fill @e3 "text"; agent-browser click @e5; agent-browser wait --text "Saved"
agent-browser get text main; agent-browser screenshot shot.png
agent-browser close                  # ALWAYS when done: closes your tabs
```

Rules:
- Pass `--cdp` only on the first command of a session. The URL carries this Mac's secret: never print it into
  logs, PRs, chats or files. Only Claude Code, Codex, Cursor and Paseo processes are accepted.
- If `chrome-relay` is missing or a connect fails, run `chrome-relay doctor` and tell the person what it says.
  Setup is theirs to do (README of github.com/aindeev/agent-chrome-relay); never install it for them.
- Google sign-in hops are automatic when the account is already signed in (account chooser, Continue/Allow).
- Never type credentials you know or pick accounts yourself. The only credentials you may use are the ones in
  the 1Password vaults the person allowed (below), if they set that up. Otherwise, if a command fails with `SIGN-IN NEEDED: ...` (password,
  passkey, 2-step, signed-out account) or you land on a login form: stop, ask the person to sign in in their
  own Chrome window for that profile, and retry after they confirm.
- Popups and "open in new tab" links open as new hidden tabs that belong to you (`agent-browser tab list`).
- Everything you do is in the audit trail (`chrome-relay audit`). Typed text is stored only as its length.
- Never use `--auto-connect`, `--cdp 9222`, `--headed`, Playwright attached to their Chrome, computer use or
  AppleScript against Chrome: those take over their screen.
- Public pages that need no login: a plain headless `agent-browser open <url>` (no `--cdp`) is fine.

## Signing in with 1Password

If the person has set it up, you sign in with logins from the 1Password vaults they allowed, without ever
seeing a secret. See what's there (vaults, titles, websites, usernames, and whether each has a password,
one-time code or passkey; never values):

```bash
chrome-relay vault
```

Fill a field with a **secret reference** instead of a value. The relay types the real value into the page:

```bash
agent-browser fill @e2 "op://Shared with Chrome Relay/GitHub/username"
agent-browser fill @e3 "op://Shared with Chrome Relay/GitHub/password" # password fields only
agent-browser click @e4
agent-browser fill @e7 "op://Shared with Chrome Relay/GitHub/otp" # the current 2-step code (TOTP), computed when typed
```

- **Pick the account deliberately.** The person may have several logins for one site (work and personal,
  several Twilio items). Choose the item whose vault, title and username match what they asked for ("my work
  account" = their work email); if that isn't clear, ask them which, and never try one after another.
- **Two items with one title** (two "Google" logins, say) are refused: the error lists each one's username and
  a reference by id (`op://<vault>/<id>/<field>`), and `chrome-relay vault` prints the same under such items. Use
  the id of the account the person asked for; if that isn't clear, ask them.
- The person approves: Chrome Relay's own approval names you, the site, the account and the item (once per
  sign-in or once per session, as they chose). One sign-in approval covers that item's username, password and
  one-time code, on any of its websites, for 2 minutes: fill them one after another without waiting, and
  don't pause between steps (a later fill of the same item after 2 minutes asks again). It goes to Touch ID on the Mac
  and, if they paired one, Face ID on their iPhone; either answers. Tell them it's coming; the command waits for
  them. If they decline, the command fails with `1Password: the person did not approve ...` or `... denied it
  on their iPhone`: stop and ask.
- Field names: `username`, `password`, `otp` (the one-time code), or any other field by its name. The vault
  is one of the vaults `chrome-relay vault` lists.
- Use `fill` (one reference, the whole text). `type`, `keyboard` or `eval` would type the reference literally.
- A value only goes to the item's own website (and its subdomains), over https, into the page itself (not a
  frame inside it). Passwords and other secret fields only go into password fields. A refusal comes back as
  `1Password: ...`: tell the person what it says; don't work around it.
- Never try to read a secret back (eval of a field's value, screenshots of an unmasked field, copying it
  elsewhere), print it, or paste a reference into anything but a sign-in field. Values that come back through
  the relay are masked anyway, and every fill, refusal and passkey use is in `chrome-relay audit`.
- Not set up (`chrome-relay vault` says so), or their Chrome Relay was built without 1Password? It's optional,
  and setting it up is the person's job (`chrome-relay vault setup`): ask them, never do it yourself.

## Saving new logins and 2-step codes

When the person asks you to create an account or turn on 2-step verification, Chrome Relay saves the secret to
1Password itself. You name it with a reference ending in `?new`, and never see the value:

```bash
# sign-up: Chrome Relay makes a strong password, saves the login, types it
agent-browser fill @email "bot@acme.com"
agent-browser fill @pw "op://Shared with Chrome Relay/Acme/password?new&username=bot@acme.com"
agent-browser fill @pw2 "op://Shared with Chrome Relay/Acme/password?new&username=bot@acme.com"  # "confirm": the same one
# 2-step verification: on the page that shows the QR code, Chrome Relay reads the key, saves it, types the code
agent-browser fill @code "op://Shared with Chrome Relay/Acme/otp?new"
```

- **Pick the vault deliberately**: the account's (a work sign-up into the work vault), as `chrome-relay vault`
  lists them. If it isn't clear, ask.
- The person approves each save (Touch ID or their iPhone), seeing the site and the account. The new login is a
  Login item titled as you named it, for the page's site, tagged `chrome-relay-saved`.
- Password rules: add `&length=16` (12 to 64) or `&symbols=no` if the site rejects the default (24 characters with
  symbols). After a refusal, ask the person rather than trying again: the first password is saved already.
- One login per site and account: if one exists, the save is refused and names its reference; fill that instead.
  Changing an existing password is the person's job.
- 2-step setup: the page must show the key, as a QR code or as text (many sites hide the text key behind "can't
  scan?" or "enter a setup key": click that first if the save says there's no key). On a login Chrome Relay saved,
  the key goes into it; on the person's own login, into a separate item "<item> one-time code · Chrome Relay"
  beside it (Chrome Relay never changes the person's items). From then on fill `op://<vault>/<that item>/otp`.
- Don't read the setup key or the backup codes off the page into your output. Backup (recovery) codes aren't saved
  for you: tell the person to keep them.

## Passkeys

Passkeys need no command: when a page asks for one (a "Sign in with a passkey" button), your tab answers it.
Which way depends on what the person set up, and none of it is yours to change (`chrome-relay passkeys`,
`chrome-relay vault`: never run their `on`, `off`, `reuse` or `setup` forms yourself).

- **A passkey in the vaults they allowed, made by Chrome Relay**: the relay signs with it in this Mac's Secure
  Enclave, once the person approves: Touch ID on the Mac or Face ID on their paired iPhone, naming you, the
  site and the account. Tell them it's coming, and wait for the page. If they turned on `chrome-relay passkeys
  reuse`, one approval covers your passkey sign-ins for a while (the prompt says how long). If they decline, the page reports it: don't retry without asking.
  If the vault has several passkeys for the site and the page doesn't say which, the request fails and names
  them: fill the username of the one you mean first (`agent-browser fill @ref "op://<vault>/<item>/username"`).
- **Creating one** (a site's "Create a passkey" button, right after you clicked or typed in the page): the new
  passkey goes into the Secure Enclave and the vault, with no approval. Not while the Mac is locked: then the
  page is refused, and you ask the person to create it when they're back.
- **A passkey only 1Password holds** (the person's own, e.g. their Google passkey), or no vault set up: the
  request goes to 1Password's prompt in your tab, which only the person can approve. Commands on that tab then
  fail after 15s with `PASSKEY APPROVAL NEEDED: ...`. The person gets a Chrome Relay notification (clicking it
  opens your tab; so does the Chrome Relay toolbar icon, which shows a red "key" badge). Tell them to approve in
  1Password, then poll with a cheap command (`agent-browser get url`) every ~15s. Don't retry in a loop. After
  5 minutes with no answer you get `PASSKEY APPROVAL TIMED OUT` and the tab is gone: ask before trying again.
- **Passkeys turned off** (`chrome-relay passkeys` says so): pages' passkey requests fail in your tabs. Ask the
  person to sign in another way.

**Giving an account a Chrome Relay passkey**, when the person asks for it ("set up a passkey for my GitHub").
Afterwards you can sign in to that account with only their approval (Touch ID or Face ID), no password or code.
Do one account at a time, and tell the person before each step that needs them:
1. Open the site's passkey settings (Google: `https://myaccount.google.com/signinoptions/passkeys`; GitHub:
   `https://github.com/sessions/trusted-device?return_to=%2Fsettings%2Fsecurity`, where "Add passkey" on
   `https://github.com/settings/security` leads). Sites first make you prove it's you. That is usually a
   passkey only 1Password holds (see above: the person approves it), or a password and 2-step code. Those
   you fill from the vault if the item is there, else the person signs in.
2. **Stay in that tab.** Google's "verify it's you" counts for that page only: a new tab or session asks again,
   which costs the person another approval. Once the settings page is back, `agent-browser reload` it once (the
   proof stays in the URL), so Chrome Relay's page scripts load before the site's.
3. If `chrome-relay vault` already shows a passkey for this site, account and Mac, stop: don't create a second.
4. Click "Create a passkey" / "Add a passkey". Google then shows its own step: click "Continue passkey
   enrollment" (not "another device"). The relay makes the passkey at once (within 30s of your click) and the
   site says it's created; `chrome-relay audit 5 passkey` shows "passkey created".
5. Sites guess its name from the Mac (Google lists it as "iCloud Keychain"): rename it "Chrome Relay" (the
   edit/pencil button; Google allows 20 characters) so the person can tell it apart. Google may also show
   "Approve this passkey?" (a security delay): that's the person's to approve, with their own passkey.
6. `chrome-relay vault` lists it as "<site> passkey (<username>) · Chrome Relay · <this Mac>", in the vault beside
   that account's login. Passkeys are per Mac: the person may use Chrome Relay on several Macs with the same
   vaults, and each Mac signs only with its own. One made on another Mac doesn't count here; if the site already
   lists a "Chrome Relay" passkey from another Mac, add one for this Mac and name it after the Mac if the site
   allows (e.g. "Chrome Relay MBP"). Check the site lists it too: "passkey created" means the relay made it, not that the site
   accepted it (if the site says registration failed, stop and tell the person).
If Chrome's or macOS's own "Save a passkey" / "Choose where to save" dialog ever appears, the relay didn't get
the request: stop and tell the person (they press Cancel); don't click again.
Site notes:
- **Google**: "verify it's you" is a passkey only 1Password holds; after "Create a passkey", click "Continue
  passkey enrollment"; rename it "Chrome Relay" (pencil button); the person may need to approve it ("Approve
  this passkey?").
- **GitHub**: its sudo check ("Confirm access") offers a passkey, an authenticator app or an email code, no
  password: choose the authenticator app and fill the vault's `otp`, then click "Verify" (it doesn't submit by
  itself). Sudo lasts about 2 hours, so the person often isn't asked at all. After "Add passkey" GitHub says
  "Successfully added a passkey" and asks for a nickname: "Chrome Relay", then "Done". `settings/security` then
  counts one more under "Passkeys".
- Sites without passkeys for their own sign-in (Twilio today), or whose 2-step slots are full (Float allows two
  methods): say so and stop; never remove a method to make room.
Never remove or replace the person's other passkeys or sign-in methods, and never click account recovery.
