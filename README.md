<p align="center"><img src="docs/icon.png" width="96" alt="Chrome Relay icon"></p>

# Chrome Relay

<p align="center">
  <a href="https://agent-chrome-relay.aindeev.com"><img alt="Website" src="https://img.shields.io/badge/website-agent--chrome--relay.aindeev.com-0b0d10?style=flat-square"></a>
  <a href="https://read.aindeev.com"><img alt="Blog: The Leveraged Mind" src="https://img.shields.io/badge/blog-The%20Leveraged%20Mind-ff6719?style=flat-square&logo=substack&logoColor=white"></a>
  <a href="https://x.com/AlexeyIndeev"><img alt="X: @AlexeyIndeev" src="https://img.shields.io/badge/X-@AlexeyIndeev-000000?style=flat-square&logo=x&logoColor=white"></a>
  <a href="https://www.linkedin.com/in/alexey-indeev/"><img alt="LinkedIn: Alexey Indeev" src="https://img.shields.io/badge/LinkedIn-Alexey%20Indeev-0a66c2?style=flat-square&logo=linkedin&logoColor=white"></a>
  <a href="LICENSE"><img alt="MIT License" src="https://img.shields.io/badge/license-MIT-2ea44f?style=flat-square"></a>
</p>

**Website:** https://agent-chrome-relay.aindeev.com


Lets your coding agents (Claude Code, Codex, Cursor, Paseo) use **your everyday Chrome**, with your real
logins, from the command line, **in background tabs**: nothing takes focus, no dialogs, ~0.15s per command.

```bash
agent-browser --cdp "$(chrome-relay url you@company.com)" open https://app.example.com/
agent-browser snapshot -i          # the page's buttons, links and fields, each with a ref like @e5
agent-browser click @e5
agent-browser close
```

It's opt-in: nothing installs the extension for you, and it only runs on Macs whose owner set it up.

**The core** is background tabs with your logins, Google sign-in hops and an audit trail. **Everything else is
optional** and off until you turn it on: signing in with 1Password (passwords, 2-step codes, passkeys) and
Secure Enclave passkeys. See [What's optional](#whats-optional).

## How it works

```
agent-browser (CLI) ──ws──▶ relay (127.0.0.1:9333, LaunchAgent) ──ws──▶ Chrome Relay extension ──chrome.debugger──▶ agent tab
```

- **Extension** (`extension/`, MV3, one per Chrome profile you load it in): opens each agent tab **inactive**
  in a collapsed "Agents" tab group and runs DevTools commands in it via `chrome.debugger`. No remote-debugging
  port, so Chrome never shows "Allow remote debugging?". Chrome does show "Chrome Relay started debugging this
  browser" while agents work.
- **`chrome-relay`**: one Go program, **built and signed on your own Mac** (`./build`). As a background service
  (`chrome-relay serve`) it is the relay: a DevTools endpoint per agent, where each agent sees and controls only
  the tabs it opened and commands that could raise or focus the browser are swallowed. It is also the CLI
  (setup, the connection URL, diagnostics, audit trail), and the optional 1Password vault and passkeys, whose
  Secure Enclave and Touch ID code (`internal/macos`, Swift) is linked into the same signed binary when Xcode's
  command line tools are installed.

## Setup (once per Mac, ~2 minutes)

Fastest: paste [this prompt](https://agent-chrome-relay.aindeev.com/setup-prompt.txt) into Claude Code, Codex or
Cursor and let your agent do it. By hand:

Needs macOS, Google Chrome, [Go](https://go.dev/dl/) 1.25+ (`brew install go`) and
[`agent-browser`](https://github.com/vercel-labs/agent-browser) (`npm i -g agent-browser`).

```bash
git clone https://github.com/aindeev/agent-chrome-relay
cd agent-chrome-relay
./build                    # compiles chrome-relay and signs it on this Mac (see below)
./chrome-relay setup       # secret, background service, identities, `chrome-relay` on PATH, Claude Code skill
```

**Why you build it yourself**: the binary is signed on your Mac, and macOS ties Chrome Relay's Keychain item to
that signature, so only your own build can read it (see the 1Password section). The build uses the hardened
runtime, so other programs can't attach a debugger to the relay or inject code into it. Two things make the
build fuller, and neither is required:

| You have | `./build` makes | |
|---|---|---|
| Go only | the **core**: tabs, Google sign-in hops, audit trail | passkeys you keep in 1Password still work (1Password's prompt) |
| + Xcode's command line tools (`xcode-select --install`) | the core **plus 1Password and Secure Enclave passkeys** | |
| + a code signing identity | the same, **signed by you** | without one it signs ad-hoc: macOS may ask once after each rebuild before Chrome Relay reads its own Keychain item |

Any Apple Development or Developer ID certificate is an identity (`security find-identity -v -p codesigning`
lists yours; a free one comes with an Apple ID in Xcode → Settings → Accounts → Manage Certificates). With
several, pick one with `./build --identity "<name>"`; it's remembered. `chrome-relay doctor` says which build
you have.

Then, **in each Chrome profile agents should use**: open `chrome://extensions`, turn on **Developer mode**,
**Load unpacked**, and pick the `extension` folder of your clone. Check with `chrome-relay doctor`.

After `git pull`: `./build && chrome-relay update` (rebuilds, restarts the relay, reloads the extension in every
profile).
To remove: `chrome-relay uninstall`, then remove the extension in each profile.

## Telling your agents

Claude Code: `setup` links the skill (`skill/SKILL.md`) into `~/.claude/skills/chrome-relay`, so every
repo gets it. Codex, Cursor and others: point them at the same file, e.g. a line in your global
`~/.codex/AGENTS.md`: *"For any browser step that needs my logins, follow
`<path>/agent-chrome-relay/skill/SKILL.md`."* A repo's `CLAUDE.md`/`AGENTS.md` only needs a
one-line pointer to the skill.

## Who can drive it

Two checks on every agent connection:

1. **This Mac's secret**: generated at setup in `~/.chrome-relay/token` (mode 600) and embedded in the URL
   that `chrome-relay url` prints.
2. **An allowed agent app**: the connecting process must come from Claude Code, Codex, Cursor or Paseo. The
   relay looks the process up (`lsof`, `ps`) and checks the markers those apps set in their child processes
   (`CLAUDECODE`, `CODEX_*`, `CURSOR_*`, `PASEO_AGENT_ID`), then its parent processes.

Connections that carry a browser `Origin` header (a web page) are refused even with the secret. Only **this
checkout's extension** may connect as the extension: Chrome derives an unpacked extension's ID from its folder,
`setup` records that ID in `~/.chrome-relay/config.json`, the relay checks the connection's
`Origin: chrome-extension://<id>` (which pages cannot fake), and the connecting process must be Chrome.

This keeps other local tools, web pages and accidents out. It is not a boundary against malware already
running as you, which could read the secret and fake the markers.

## Sign-ins

Agents never type credentials they know. When a site bounces through Google and the right account is **already signed
in**, the extension clicks the account and **Continue/Allow** itself (page scripting, so it works even with
1Password's frame on the page). A password, passkey, 2-step screen or signed-out account stops the hop, and the
agent gets `SIGN-IN NEEDED: ...` and asks you to sign in in your own window.

Which Google account a site uses comes from `~/.chrome-relay/identities.json` (`chrome-relay identities`):
your Chrome profiles (filled in by setup), optional hand-set `sites.rules`, and `sites.learned`, which the
relay fills in after each successful hop. Default: the profile's own account.

## What's optional

Nothing below happens unless you set it up, and each part can be turned off again:

| Feature | Default | Turn on | Turn off |
|---|---|---|---|
| Background tabs, Google sign-in hops, audit trail | on | `./chrome-relay setup` | `chrome-relay uninstall` |
| Signing in with 1Password (passwords, 2-step codes) | **off** | `chrome-relay vault setup` | `chrome-relay vault forget` |
| Passkeys in agent tabs | on: 1Password's own prompt answers, or Chrome Relay's Secure Enclave passkeys if you have 1Password set up | `chrome-relay passkeys on` | `chrome-relay passkeys off` |
| One Touch ID for several passkey sign-ins | **off** (every signature asks) | `chrome-relay passkeys reuse 120` (1–300 s) | `chrome-relay passkeys reuse off` |

With passkeys off, agent tabs leave pages' WebAuthn alone (Chrome then refuses it in background tabs, as it
would anyway). With no vault set up, the relay never talks to 1Password, prompts for nothing and stores no
token.

## Signing in with 1Password (optional)

Needs a build with Xcode's command line tools (see Setup).

Agents can sign in with logins from 1Password: passwords, 2-step codes (TOTP) and passkeys. They never see a
value: they name an item, and the relay types the value into the page, only on that item's website.

```bash
chrome-relay vault setup     # pick your 1Password accounts and which of their vaults agents may use
chrome-relay vault           # what agents can see: vaults, titles, websites, usernames, what each item holds
```

**Through your 1Password app (the default).** Setup lists the 1Password accounts on this Mac (several at once
are fine, e.g. personal and work), shows each one's vaults, and you tick the ones agents may use, say *Private*
and *Shared with Chrome Relay*. Nothing is stored: the relay connects through the 1Password app (1Password's
Go SDK; turn on *Settings → Developer → Integrate with other apps*), and **1Password asks you to allow it** when
the relay first needs an account, and again after about 10 idle minutes or when 1Password locks. Vaults you
didn't tick stay out of reach, and so does every item outside its own website.

**Through a service account**, for a vault shared with agents that should work without you (a bot user, a
staging admin): `chrome-relay vault setup <vault>` asks for the token of a
[service account](https://developer.1password.com/docs/service-accounts/) with access to **that vault only**
(*Read Items*, plus *Write Items* if agents may save new passkeys there). 1Password itself then keeps everything
else out of reach.

Agents fill a field with a [secret reference](https://developer.1password.com/docs/cli/secret-reference-syntax/)
and the relay types the real value:

```bash
agent-browser fill @e2 "op://Private/Twilio/username"
agent-browser fill @e3 "op://Private/Twilio/password"
agent-browser fill @e7 "op://Private/Twilio/otp"      # the current one-time code, computed from the item's TOTP secret
```

- **Which account.** You may have several logins for one site (three Twilio items, say). The relay never picks:
  the agent names the item, choosing by the vault, title and username `chrome-relay vault` shows it, and asks you
  when your request doesn't say which.
- **Chrome Relay asks too, with the details.** Its own Touch ID prompt names the agent, item and site:
  *"chrome-relay is trying to let Claude Code (my-repo) fill the password of "Twilio" on www.twilio.com"*. How
  often is yours to choose in setup: **once per agent session** (the default with the 1Password app: one tap,
  naming the vaults, covers that agent's sign-ins until it disconnects), **every sign-in** (one tap covers that
  agent, item and site for a minute: username, password, one-time code), or **never** (1Password's prompt only).
  Decline anything you didn't expect.
- **Only the item's site.** A value is typed only into a page whose host is the item's website or a subdomain
  of it, over https (or `localhost`), in the page itself rather than a frame inside it, like 1Password's own
  filling. References to any other vault are refused.
- **Secrets only into password fields**, so they show as dots on screen and in the agent's snapshots. If the
  agent (or the page) reads a filled secret back, the relay masks it in everything it sends the agent.
- **Passkeys, in this Mac's Secure Enclave.** Chrome won't run WebAuthn in a background tab, and 1Password's
  passkey prompt would knock the debugger off the page, so agent tabs hand `navigator.credentials.create/get` to
  the relay, which acts as the authenticator (ES256, no attestation). The keys themselves are made by the Mac's
  **Secure Enclave** and never leave it: `chrome-relay` asks the chip to create a key or sign, and **every
  signature needs your Touch ID**. The prompt says who is asking, for which site and account: *"chrome-relay is
  trying to let Claude Code (my-repo) sign in to github.com as bot@company.com, from github.com"*. Neither the
  relay, the vault nor the agent ever holds a private key. If an agent signs in to many sites in a row,
  `chrome-relay passkeys reuse 120` lets one Touch ID cover **that agent's** passkey sign-ins for 2 minutes (up to
  300 seconds; the prompt says so; other agents still ask). Off by default. A site's "Create a passkey" (only right after the agent clicked or typed in the page) stores the
  public key and the chip's handle in the vault (on the item for that site with the same username, else a new
  Login item, in a *Passkey (Chrome Relay)* section); the handle is useless on any other Mac, so such a passkey
  works on the Mac that made it.
- **Your own 1Password passkeys** (your Google passkey, say) never leave 1Password: no program can read them.
  When a page asks for a passkey the relay doesn't hold, the request goes to 1Password's own prompt in the
  agent's tab, and you approve it. The Chrome Relay icon shows a red **key** badge; clicking it opens that tab
  (the only time an agent tab comes forward, and only on your click). The tab waits up to 5 minutes, and the
  agent is told `PASSKEY APPROVAL NEEDED` so it can ask you. This works with no vault set up at all.
- **Audited.** Every fill, refusal, passkey creation and passkey sign-in is in `chrome-relay audit` (item and
  field names, never values).
- **Signing.** Loading the 1Password app's library into the relay needs the `disable-library-validation`
  entitlement (the library is signed by AgileBits, not you); debuggers and `DYLD_*` injection stay blocked.

**Where a service account's token lives** (the 1Password app needs none). In your login Keychain (item
*Chrome Relay*), in two layers:

1. **Only your signed `chrome-relay` reads the item.** It creates the item itself, so macOS ties it to the
   binary's signature: any other program that asks (`security`, a script, an agent) gets macOS's *"… wants to
   access 'Chrome Relay' in your keychain"* dialog, and nothing unless you allow it. (Most command-line tools,
   Claude Code's and Codex's own logins included, store theirs through Apple's `security` tool, which every
   program on your Mac can run silently. Chrome Relay deliberately doesn't.)
2. **What's in the item is locked to this Mac's Secure Enclave.** Even a program you let through gets only
   ciphertext: opening it takes the chip and your Touch ID.

The relay talks to 1Password in-process (no `op` command, so the token never sits in another process's
environment), keeps the token in memory only while it runs, and is built with the hardened runtime, so no
debugger can attach to it. `chrome-relay vault forget` removes the item. One thing this can't change: an agent
can always *use* the relay as intended, filling a site's password into that site, where page script could read
it. So keep in the vault only accounts you're happy for agents to use, and give the service account an expiry.

## Audit trail

Every agent action is recorded in `~/.chrome-relay/audit/YYYY-MM-DD.jsonl`; read it with `chrome-relay audit`:
who connected (app, Claude/Paseo session id, working folder, `AGENT_BROWSER_SESSION`), pages, clicks with
positions, special keys, the agent's own scripts, screenshots, uploads, popups, sign-in clicks and blocked
pages. Typed text is stored only as a character count, and URLs lose their query string.

## Behaviour worth knowing

- **Popups and `target=_blank` links** open as new hidden tabs owned by the same agent; popup sign-in flows
  that `postMessage` back to the opener and close themselves keep working. Message origins are the ones the relay
  saw each tab load (never what the page claims), and `targetOrigin` is enforced. A safety net adopts any tab an agent
  page opens anyway and hands focus back to the tab you were on.
- **1Password and other password managers** stay out of agent tabs: their fields are marked `data-1p-ignore`
  (and the LastPass and Bitwarden equivalents), because Chrome drops an extension's debugger from a page that
  contains another extension's frame (1Password's inline menu). If a frame gets in anyway, the agent's commands
  wait while the extension re-attaches.
- **Self-cleaning**: tabs of an agent silent for 30 minutes are closed. When the relay restarts, the extension
  closes the agent tabs it opened (tracked by tab id, so your own tab groups are never touched).
- **Real input in background tabs**: focus emulation is on in every agent tab, so clicks and typing land.

## Development

**Dev mode**: try a branch beside your normal install. From a second checkout (e.g. a git worktree):
`./build && ./chrome-relay setup --dev` installs `chrome-relay-dev-mode`, with its own relay (port 9334, written to the
checkout's `extension/relay.json`), data folder `~/.chrome-relay-dev-mode`, background service and Keychain
item; it leaves your agents' skill alone. Load that checkout's `extension` folder unpacked as a second Chrome
Relay (its tooltip says dev mode), and point agents at `chrome-relay-dev-mode url <email>`. Remove it with
`chrome-relay-dev-mode uninstall`.

```bash
go generate ./internal/macos            # the Swift Secure Enclave / Keychain / Touch ID layer (./build does this too)
CGO_LDFLAGS="-L$(xcrun --show-sdk-path)/usr/lib/swift -L$(dirname $(xcrun -f swiftc))/../lib/swift/macosx" \
  go test -race ./...                   # relay, CLI, 1Password fills, passkeys (in-memory 1Password and chip stand in)
CGO_ENABLED=0 go test ./...             # the core-only build (also what runs on Linux)
node --test test/extension-*.test.mjs   # the extension's service worker
```

Logs: `chrome-relay logs` (`~/.chrome-relay/relay.log`). Testing extension changes: edit `extension/`, then
`chrome-relay update`. `CHROME_RELAY_HOME` and `CHROME_RELAY_PORT` override the data folder and port.

## About the author

Made by **Alexey Indeev**, co-founder and CTO of [Spare](https://sparelabs.com). I write about building more
with AI, whether you code or not, at **[The Leveraged Mind](https://read.aindeev.com)**.

- Blog: https://read.aindeev.com
- X: [@AlexeyIndeev](https://x.com/AlexeyIndeev)
- LinkedIn: https://www.linkedin.com/in/alexey-indeev/
- GitHub: [@aindeev](https://github.com/aindeev)

Questions, ideas or bugs: open an [issue](https://github.com/aindeev/agent-chrome-relay/issues) or reach out on X.

## License

MIT. See [LICENSE](LICENSE).
