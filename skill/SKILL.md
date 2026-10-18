---
name: chrome-relay
description: Use the person's own signed-in Chrome from the command line, in background tabs, through Chrome Relay. Load this BEFORE any browser step that needs their logins (internal tools, Google-SSO apps, GitHub, admin consoles, staging environments, OAuth consent screens, sign-ins with the 1Password vaults they allowed: passwords, 2-step codes, passkeys) or when they say "check it in the browser", "open it logged in", "click through", "use my Chrome". Never drive their Chrome any other way.
---

# Chrome Relay

Chrome Relay lets Claude Code, Codex, Cursor and Paseo agents drive **background tabs** in the person's
everyday Chrome, with their real logins. Your tabs open inactive in a collapsed "Agents" tab group, you only
see and control tabs you opened, and nothing on their screen moves. Tens of milliseconds per command.

```bash
export CHROME_RELAY_SESSION=<your-agent-name>                   # one session per agent
chrome-relay identities                                         # which profile (email) to use for what
chrome-relay open https://example.internal/ --profile you@company.com   # the first open starts your session
chrome-relay snapshot                # the page's controls, each with a ref like @e5 (text, cheap)
chrome-relay fill @e3 "text"; chrome-relay click @e5; chrome-relay wait "text:Saved"
chrome-relay get text main; chrome-relay screenshot shot.png
chrome-relay close --all             # ALWAYS when done: closes your tabs
```

For anything more than a click or two, use `chrome-relay do "<goal>" --data ... --until ...` (below).

A target is a ref from your last snapshot (`@e5`), a control's whole label (`"Save"`, or `button:Save` when
two kinds share it), or a CSS selector (`#save`, `css:main button`). A label matching two controls is refused,
never guessed: use the ref it suggests. Every action first checks the control is still there, enabled, on screen
and not covered, and says what changed. Refs don't carry over to a new page: snapshot again after it changes.
`chrome-relay help` lists the rest: `select`, `check`/`uncheck`, `type`, `press Enter`, `hover`, `dblclick`,
`drag`, `upload`, `scroll`, `back`, `reload`, `get value|html|attr|count|url|title`, `is visible|enabled|checked`,
`eval`, `pdf`, `console`, `errors`, `cookies`, `dialog accept|dismiss`, `tabs`, `tab <id>`, `downloads`, `save`, `cdp <Domain.method>`.

Rules:
- Only Claude Code, Codex, Cursor, Paseo and pi processes are accepted. Agents that set no session id of their
  own (Cursor) must set `CHROME_RELAY_SESSION`.
- If a connect fails, run `chrome-relay doctor` and fix what it reports yourself (it says how: `chrome-relay
  update`, restarting the service, ...). If `chrome-relay` isn't installed, set it up by following
  https://agent-chrome-relay.aindeev.com/setup-prompt.txt; only loading the extension is the person's to do.
- **Finish the job; don't hand it back.** The person installed Chrome Relay so you carry their task through
  to the end without them, fast. Signing in is part of the task, and they have authorized you to do it with
  everything below. Asking them is the last resort, not the default. On a sign-in page or a `SIGN-IN NEEDED`
  error, go down this list and use the first that works:
  1. Already signed in: Google account choosers and Continue/Allow are clicked for you.
  2. 1Password (`chrome-relay vault`): fill the username, password and 2-step code from the item for the site
     (below). A code split over several one-character boxes: fill the first box, and the relay types one
     character per box.
  3. A code or link sent by email or text message ("we sent a code to a•••@..."): read it yourself, from the
     person's mail in a tab of yours (Gmail) or a mail tool you have, and type the code (or open the link) in
     your tab. The person has authorized this: don't ask them to copy it over, and don't refuse.
  4. "Remember this device" / "Trust this browser" / "Don't ask again": tick it, so next time skips the code.
  5. Signed in with a password or a code? If 1Password is set up and the site offers passkeys, give the account
     a Chrome Relay passkey right away, in the same tab (see "Giving an account a Chrome Relay passkey"), so
     your next sign-in is one approval, with no password, code or email. Mention it in your reply.
  Only when none of these works (no item in the vault, a passkey only 1Password holds, an account they never
  shared) ask the person, saying exactly what you need ("add Wagepoint to the vault", "approve in 1Password"),
  and carry on with everything else meanwhile. Before asking, try every other way in the page offers ("Try
  another way", "Use your password instead", "Send a code by email"): the person is bothered only when you're
  out of options.
- Passwords only as 1Password references (`op://...`), never as text you type: a password in your command
  would end up in your transcript. Pick the account that matches the person's request.
- Popups and "open in new tab" links open as new hidden tabs that belong to you (`chrome-relay tabs`, then
  `chrome-relay tab <id>`). Never replace `window.open` or stub popups yourself: the relay already routes them,
  and a stub breaks the page. A page's alerts are accepted; a confirm or prompt is dismissed unless you said
  beforehand `chrome-relay dialog accept [text]`.
- Everything you do is in the audit trail (`chrome-relay audit`). Typed text is stored only as its length.
- Never use remote debugging (`--remote-debugging-port`, `--cdp 9222`, `--auto-connect`), Playwright attached to
  their Chrome, computer use or AppleScript against Chrome: those take over their screen.
- agent-browser still works through the relay if you prefer it: `agent-browser --cdp "$(chrome-relay url
  you@company.com)" open <url>` on the first command (the URL carries this Mac's secret: never print it into logs,
  PRs, chats or files), then plain `agent-browser ...`. Don't mix the two in one task.

## Downloading files (statements, invoices, exports)

Files a page hands over come to you, never to Chrome's Downloads: a download link, a PDF, an export the page's
script builds, and a popup that turns into a file (a bank's blank popup that gets a signed link from its API).
Click with `--download` and say where they go:

```bash
chrome-relay click "March statement" --download --into ~/dev/finance/wealthsimple   # waits for it (--timeout 30)
chrome-relay downloads [--into <dir>] [--wait 30]   # every file this session got; --into moves them there
chrome-relay open https://example.com/report.pdf --into .                            # an address that is a file
chrome-relay save - --into <dir> < links.txt   # addresses loaded with the tab's cookies, one "<url> [name]" a line
```

- Every action lists the files it brought (`↓ saved …`). Without `--into` they wait in the session's folder,
  which is deleted when the session ends: move them before `close --all`.
- Many documents: click each one with `--download`, or collect their links in the page (`eval`) and pipe them
  into `save -`. Don't build login tokens from cookies or fetch files with a script outside the browser, and
  don't write signed links to files: they're secrets until they expire (`save` never prints their query).
- `↓ blocked …`: the page started a file on its own, not within a minute of your own click: click what makes it.
  No file came? `chrome-relay tabs` shows a popup still open; `chrome-relay errors` says what the page threw.

## Whole goals in one command: `chrome-relay do`

Once the page is open, hand Chrome Relay the goal instead of clicking step by step. It fills what you name,
replays what worked on this site before, asks a fast decision model for the rest, and checks every action. It
usually takes a few seconds where step-by-step takes minutes.

```bash
chrome-relay do "Invite the new teammate as an admin" \
  --data "Email=ada@example.com" --data "Role=Admin" --until "text:Invitation sent"
```

- **Give every value you know as `--data <field label>=<value>`**, keyed by the label the page shows ("Email",
  "Project name"); it never invents values. Credentials only as `op://` references
  (`--data "Password=op://<vault>/<item>/password"`), never in the goal text.
- **Always give `--until`**: what the page shows when it's done (`text:...`, `url:...`, `title:...`,
  `no-text:...`, `selector:...`). Only then is "done" verified, and only then is the run saved as a flow that
  repeats it without a model next time.
- **Exit 0** = done (the output lists every step). **Exit 2** = handed back: it says why, what it did, the page
  it's on and the elements it was choosing between. Continue from there yourself (`chrome-relay snapshot`, then
  click or fill); don't rerun the same `do` hoping for a different result. It hands back
  on purpose before clicking a consequential button the goal doesn't name (delete, send, pay, deploy, ...): if
  the person asked for exactly that, rerun with `--allow <verb>`; never `--allow '*'` on your own.
- One goal per run, in the tab you used last (`--tab <id>` for another), or a new page with `--url <url>
  [--profile <email>]`. Same `CHROME_RELAY_SESSION` as the commands that opened it.
- `chrome-relay decider` says what it decides with (Jev, Haiku through Claude Code, or none). Setting a key is the
  person's job; never run `decider jev` yourself.

## Signing in with 1Password

If the person has set it up, you sign in with logins from the 1Password vaults they allowed, without ever
seeing a secret. See what's there (vaults, titles, websites, usernames, and whether each has a password,
one-time code or passkey; never values):

```bash
chrome-relay vault
```

Fill a field with a **secret reference** instead of a value. The relay types the real value into the page:

```bash
chrome-relay fill @e2 "op://Shared with Chrome Relay/GitHub/username"
chrome-relay fill @e3 "op://Shared with Chrome Relay/GitHub/password" # password fields only
chrome-relay click @e4
chrome-relay fill @e7 "op://Shared with Chrome Relay/GitHub/otp" # the current 2-step code (TOTP), computed when typed
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
- Use `fill` (one reference, the whole text). `type` or `eval` would type the reference literally.
- **2-step codes in separate boxes** (six inputs for six digits): `fill` the first box with the `otp`
  reference. The relay types one character per box. The same works for a code you read from an email: `fill`
  the first box with the whole code. Then click Verify/Continue if the page doesn't submit by itself.
- A login's value only goes to the item's own website (and its subdomains), over https, into the page itself
  or a frame of its own origin (Apple's sign-in form), never another site's frame. Passwords and other secret fields only go into password fields. Cards: see below. A refusal comes back as
  `1Password: ...`: tell the person what it says; don't work around it.
- Never try to read a secret back (eval of a field's value, screenshots of an unmasked field, copying it
  elsewhere), print it, or paste a reference into anything but a sign-in field. Values that come back through
  the relay are masked anyway, and every fill, refusal and passkey use is in `chrome-relay audit`.
- Not set up (`chrome-relay vault` says so)? Sign in with emailed codes and links if the site sends them. If
  the person uses 1Password, set it up yourself: `op account list`, then `chrome-relay vault setup "Shared
  with Chrome Relay (<short name>)" --account <email>` for each account (1Password asks them to approve once),
  and ask them to move the logins agents may use into that vault. Built without 1Password (`chrome-relay
  doctor` says so)? Rebuild with Xcode's command line tools (`./build` in the clone, then `chrome-relay update`).

## Paying with a card from 1Password

When the person asks you to buy something with a card they keep in the vault (a 1Password **Credit Card** item;
`chrome-relay vault` lists it), fill the checkout's card fields with references, one field each:

```bash
chrome-relay snapshot                     # card fields in Stripe's (or another provider's) frame show as in-frame (js.stripe.com)
chrome-relay fill @e12 "op://Shared with Chrome Relay/Venn card/number"
chrome-relay fill @e13 "op://Shared with Chrome Relay/Venn card/expiry"   # MM/YY; expiry-month and expiry-year for split fields
chrome-relay fill @e14 "op://Shared with Chrome Relay/Venn card/cvc"
chrome-relay fill @e15 "op://Shared with Chrome Relay/Venn card/name"     # if the form asks for the name on the card
```

- **The person approves every payment**: Touch ID (or their iPhone) names you, the shop and the card's last four
  digits. One approval covers that card's fields on that site for 2 minutes. Tell them it's coming.
- Card details go only into the checkout page itself (https) or a payment provider's frame in it (Stripe,
  Braintree, Adyen, Checkout.com, Square, PayPal, ...), and only into the matching field: the number into the card
  number field, the code into the security-code field. A refusal (`1Password: ...`) says why; don't work around it.
- Only use the card the person named, for the purchase they asked for. Check the amount and what's in the cart
  before you fill, and don't click Pay / Buy / Subscribe without their go-ahead if anything differs.
- Nothing you fill comes back: card fields read back as `(filled)`, and after a card fill that tab takes no
  screenshots, snapshots or PDFs until the page moves on (they would show the card). Read the page's text instead.

## Saving new logins and 2-step codes

When the person asks you to create an account or turn on 2-step verification, Chrome Relay saves the secret to
1Password itself. You name it with a reference ending in `?new`, and never see the value:

```bash
# sign-up: Chrome Relay makes a strong password, saves the login, types it
chrome-relay fill @e4 "bot@acme.com"
chrome-relay fill @e5 "op://Shared with Chrome Relay/Acme/password?new&username=bot@acme.com"
chrome-relay fill @e6 "op://Shared with Chrome Relay/Acme/password?new&username=bot@acme.com"  # "confirm": the same one
# 2-step verification: on the page that shows the QR code, Chrome Relay reads the key, saves it, types the code
chrome-relay fill @e9 "op://Shared with Chrome Relay/Acme/otp?new"
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
Which way depends on what the person set up. Their passkey settings are theirs to change: never run
`chrome-relay passkeys on|off|reuse` or `chrome-relay vault approve` yourself.

- **A passkey in the vaults they allowed, made by Chrome Relay**: the relay signs with it in this Mac's Secure
  Enclave, once the person approves: Touch ID on the Mac or Face ID on their paired iPhone, naming you, the
  site and the account. Tell them it's coming, and wait for the page. If they turned on `chrome-relay passkeys
  reuse`, one approval covers your passkey sign-ins for a while (the prompt says how long). If they decline, the page reports it: don't retry without asking.
  If the vault has several passkeys for the site and the page doesn't say which, the request fails and names
  them: fill the username of the one you mean first (`chrome-relay fill @eN "op://<vault>/<item>/username"`).
- **Creating one** (a site's "Create a passkey" button, right after you clicked or typed in the page): the new
  passkey goes into the Secure Enclave and the vault, with no approval. Not while the Mac is locked: then the
  page is refused, and you ask the person to create it when they're back.
- **A passkey only 1Password holds** (the person's own, e.g. their Google passkey), or no vault set up: avoid it
  when you can. If the page offers another way ("Try another way", "Use your password"), and the vault has a
  password and code for the account or the site emails a code, take that instead. Otherwise the request goes
  to 1Password's prompt in your tab, which only the person can approve. Commands on that tab then
  fail after 15s with `PASSKEY APPROVAL NEEDED: ...`. The person gets a Chrome Relay notification (clicking it
  opens your tab; so does the Chrome Relay toolbar icon, which shows a red "key" badge). Tell them to approve in
  1Password, then poll with a cheap command (`chrome-relay get url`) every ~15s. Don't retry in a loop. After
  5 minutes with no answer you get `PASSKEY APPROVAL TIMED OUT` and the tab is gone: ask before trying again.
- **Passkeys turned off** (`chrome-relay passkeys` says so): pages' passkey requests fail in your tabs. Sign in
  another way (the vault's password and code, an emailed code), and don't create passkeys.

**Giving an account a Chrome Relay passkey**: do it on your own, whenever you have just signed in to an account
with a password or code and 1Password is set up (and when the person asks, "set up a passkey for my GitHub").
Afterwards you sign in to that account with only their approval (Touch ID or Face ID), no password or code.
Skip it if the site has no passkeys, if passkeys are off, or if proving it's you would need the person (a
passkey only 1Password holds): don't bother them just for this. Do one account at a time:
1. Open the site's passkey settings (Google: `https://myaccount.google.com/signinoptions/passkeys`; GitHub:
   `https://github.com/sessions/trusted-device?return_to=%2Fsettings%2Fsecurity`, where "Add passkey" on
   `https://github.com/settings/security` leads). Sites first make you prove it's you. That is usually a
   passkey only 1Password holds, or a password and 2-step code. Fill those from the vault; if the site insists
   on the person's own passkey and you weren't asked for this passkey, skip it rather than bother them.
2. **Stay in that tab.** Google's "verify it's you" counts for that page only: a new tab or session asks again,
   which costs the person another approval. Once the settings page is back, `chrome-relay reload` it once (the
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
