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
agent-browser fill @e2 "op://Private/GitHub/username"
agent-browser fill @e3 "op://Private/GitHub/password"    # password fields only
agent-browser click @e4
agent-browser fill @e7 "op://Private/GitHub/otp"         # the current 2-step code (TOTP), computed when typed
```

- **Pick the account deliberately.** The person may have several logins for one site (work and personal,
  several Twilio items). Choose the item whose vault, title and username match what they asked for ("my work
  account" = their work email); if that isn't clear, ask them which, and never try one after another.
- The person approves: 1Password may ask them to allow Chrome Relay, and Chrome Relay's own Touch ID prompt
  names you, the item and the site (once per sign-in or once per session, as they chose). Tell them it's coming;
  the command waits for them. If they decline, the command fails with `1Password: the person did not approve
  with Touch ID`: stop and ask.
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

## Passkeys

Passkeys need no command: when a page asks for one (a "Sign in with a passkey" button), your tab answers it.
Which way depends on what the person set up, and none of it is yours to change (`chrome-relay passkeys`,
`chrome-relay vault`: never run their `on`, `off`, `reuse` or `setup` forms yourself).

- **A passkey in the vaults they allowed, made by Chrome Relay**: the relay signs with it in this Mac's Secure
  Enclave. The person gets a Touch ID prompt naming you, the site and the account: tell them it's coming, and
  wait for the page. If they turned on `chrome-relay passkeys reuse`, one Touch ID covers your passkey sign-ins
  for a while (the prompt says how long). If they decline, the page reports it: don't retry without asking.
  If the vault has several passkeys for the site and the page doesn't say which, the request fails and names
  them: fill the username of the one you mean first (`agent-browser fill @ref "op://<vault>/<item>/username"`).
- **Creating one** (a site's "Create a passkey" button, right after you clicked or typed in the page): the new
  passkey goes into the Secure Enclave and the vault, with no Touch ID.
- **A passkey only 1Password holds** (the person's own, e.g. their Google passkey), or no vault set up: the
  request goes to 1Password's prompt in your tab, which only the person can approve. Commands on that tab then
  fail after 15s with `PASSKEY APPROVAL NEEDED: ...`. Tell the person to click the Chrome Relay icon in
  Chrome's toolbar (red "key" badge; it opens your tab) and approve in 1Password, then continue once they
  confirm. Don't retry in a loop.
- **Passkeys turned off** (`chrome-relay passkeys` says so): pages' passkey requests fail in your tabs. Ask the
  person to sign in another way.
