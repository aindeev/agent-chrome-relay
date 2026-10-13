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
**Load unpacked**, pick the `extension` folder of your clone, and **pin** it (puzzle-piece icon → pin) so you see its badge. Check with `chrome-relay doctor`.

After `git pull`: `./build && chrome-relay update` (rebuilds, restarts the relay, reloads the extension in every
profile).
To remove: `chrome-relay uninstall`, then remove the extension in each profile.

## Telling your agents

Claude Code: `setup` links the skill (`skill/SKILL.md`) into `~/.claude/skills/chrome-relay`, so every
repo gets it. Codex, Cursor and others: point them at the same file, e.g. a line in your global
`~/.codex/AGENTS.md`: *"For any browser step that needs my logins, follow
`<path>/agent-chrome-relay/skill/SKILL.md`."* A repo's `CLAUDE.md`/`AGENTS.md` only needs a
one-line pointer to the skill.

## Whole goals in one command: `chrome-relay do`

Driving a page one `agent-browser` command at a time costs the agent a model round trip per click: the relay
answers in ~15 ms, the agent thinks for seconds. `chrome-relay do` lets the agent state the goal once, with the
values it already knows and how to tell it's done, and Chrome Relay carries it out in the agent's own tab:

```bash
agent-browser --cdp "$(chrome-relay url you@company.com)" open https://app.example.com/team/invite
chrome-relay do "Invite the new teammate as an admin" \
  --data "Email=ada@example.com" --data "Role=Admin" --until "text:Invitation sent"
```

Each step is the cheapest thing that works:

1. **A flow that worked before.** A run that ends with its `--until` checks met is saved per site
   (`~/.chrome-relay/flows/`, steps and data keys, never values); the same kind of goal there replays at browser
   speed, with the new data. `chrome-relay flows` lists them.
2. **The agent's data.** A `--data` key that names a field (`Email` → "E-mail address") fills it, picks the
   dropdown option or ticks the box, no model involved. Values can be `op://` references: the relay types them
   from 1Password as it does for agent-browser.
3. **A fast decision model** for the rest: TypeSafe's Jev (~0.15 s a step) if you saved a key, else Claude Haiku
   through your own Claude Code (~1–1.5 s a step, on your Claude plan). It picks from a shortlist of the page's
   controls and from the agent's data only: it never invents text. The decider sees the shortlist, up to 1,500
   characters of the page's visible text and the `--data` values (never `op://` references or what they hold).

Before every action the relay checks the element is still the one decided on, enabled, on screen and not
covered; after it, it waits for the page to settle (events, not polling) and reads back what changed. Anything
it isn't sure of comes back to the agent (exit code 2) with what it did, the page it's on and the elements it
was choosing between: a low-confidence decision, a field with no value, a click on a consequential button the
goal doesn't name (delete, send, pay, deploy, ...; `--allow <verb>` permits it), a covered button, going in
circles, `--until` not met. "Done" is only trusted when `--until` holds.

Which decider runs: `chrome-relay decider` (and `doctor`). Every step is in the audit trail as `do click`,
`do fill` (the field, never the text), with `do start`/`do end` around the run.

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
| Signing in with 1Password (passwords, 2-step codes), and saving the logins agents create | **off** | `chrome-relay vault setup` | `chrome-relay vault forget` |
| Passkeys in agent tabs | on: 1Password's own prompt answers, or Chrome Relay's Secure Enclave passkeys if you have 1Password set up | `chrome-relay passkeys on` | `chrome-relay passkeys off` |
| Approving on your iPhone (Face ID), when away from the Mac | **off** | `chrome-relay phone pair` | `chrome-relay phone forget` |
| Notifications when an agent needs you (a sign-in, a passkey to approve in 1Password) | on once you allow them | `chrome-relay notify test`, then allow | System Settings > Notifications > Chrome Relay |
| One approval for several passkey sign-ins | **off** (every signature asks) | `chrome-relay passkeys reuse 120` (1–300 s) | `chrome-relay passkeys reuse off` |
| A fast decider for `chrome-relay do` (TypeSafe's Jev; page text of agent tabs then goes to TypeSafe) | **off**; Haiku through your Claude Code if it's installed | `pbpaste \| chrome-relay decider jev` | `chrome-relay decider jev forget` |

The notifications come from *Chrome Relay.app*, a small app `./build` makes and `setup` puts in `~/Applications`
(it needs Xcode's command line tools). It exists because Chrome's own notifications are often switched off in
macOS, and the extension's toolbar badge only shows while the extension is pinned. Clicking one opens the tab
that needs you. A Focus (Do Not Disturb) hides them unless you let Chrome Relay through.

With passkeys off, agent tabs leave pages' WebAuthn alone (Chrome then refuses it in background tabs, as it
would anyway). With no vault set up, the relay never talks to 1Password, prompts for nothing and stores no
token.

## Signing in with 1Password (optional)

Needs a build with Xcode's command line tools (see Setup).

Agents can sign in with logins from 1Password: passwords, 2-step codes (TOTP) and passkeys. They never see a
value: they name an item, and the relay types the value into the page, only on that item's website.

```bash
chrome-relay vault setup     # asks which 1Password accounts, then makes a shared vault and a service account in each
chrome-relay vault           # what agents can see: vaults, titles, websites, usernames, what each item holds
```

**Through 1Password service accounts.** `chrome-relay vault setup` lists the 1Password accounts on this Mac and
asks which agents may use: one, several (say personal and work) or all. In each it creates a shared vault if
there isn't one ("Shared with Chrome Relay (personal)", "… (acme)": the names must differ, since agents name vaults
only by name), and a [service account](https://developer.1password.com/docs/service-accounts/) that can read and
write **that vault only**, using your [1Password CLI](https://developer.1password.com/docs/cli/) (1Password asks
you to approve once). Each token goes straight into this Mac's Keychain, never on screen. It tells you what it
made; see or revoke it on 1Password.com (Developer > Directory > Service Accounts). Then move or copy into those
vaults the logins agents may use. Service accounts work in the background and while you're away, with no
1Password prompts after setup. 1Password never lets a service account into a Private vault, which is why agents
get their own. One account without questions: `vault setup <vault> --account <email>`; a service account you made
yourself: `--paste` takes its token.

Agents fill a field with a [secret reference](https://developer.1password.com/docs/cli/secret-reference-syntax/)
and the relay types the real value:

```bash
agent-browser fill @e2 "op://Shared with Chrome Relay/Twilio/username"
agent-browser fill @e3 "op://Shared with Chrome Relay/Twilio/password"
agent-browser fill @e7 "op://Shared with Chrome Relay/Twilio/otp"   # the current one-time code, from the item's TOTP secret
```

- **Which account.** You may have several logins for one site (three Twilio items, say). The relay never picks:
  the agent names the item, choosing by the vault, title and username `chrome-relay vault` shows it, and asks you
  when your request doesn't say which.
- **Chrome Relay asks too, with the details.** Its own approval names the agent, item and site, on Touch ID
  here and, if you paired one, Face ID on your iPhone at the same time (first answer wins; see below):
  *"let Claude Code (my-repo) sign in to www.twilio.com as you@company.com with "Twilio" from 1Password"*. How
  often is yours: **every sign-in** (the default: one approval lets that agent use that item for two minutes:
  its username, password and one-time code, on any of the item's websites), **once per agent session** (one approval, naming the vaults, covers that
  agent's sign-ins until it disconnects), or **never**. Change it with `chrome-relay vault approve
  sign-in|session|off`, which asks you first. Decline anything you didn't expect.
- **Only the item's site.** A value is typed only into a page whose host is the item's website or a subdomain
  of it, over https (or `localhost`), in the page itself rather than a frame inside it, like 1Password's own
  filling. References to any other vault are refused.
- **Secrets only into password fields**, so they show as dots on screen and in the agent's snapshots. If the
  agent (or the page) reads a filled secret back, the relay masks it in everything it sends the agent.
- **Saving what agents create.** When an agent signs up for something or turns on 2-step verification for you,
  it fills a reference ending in `?new` (`op://Shared with Chrome Relay/Acme/password?new&username=bot@acme.com`,
  `op://Shared with Chrome Relay/Acme/otp?new`). Chrome Relay generates the password, or reads the 2-step setup key
  from the page (its QR code or the key in text), saves it in 1Password, and types it, after you approve the save.
  The agent never sees either. New logins are Login items tagged `chrome-relay-saved`; a 2-step key for a login of
  yours goes into a separate item beside it, since Chrome Relay never changes your items.
- **Passkeys, in this Mac's Secure Enclave.** Chrome won't run WebAuthn in a background tab, and 1Password's
  passkey prompt would knock the debugger off the page, so agent tabs hand `navigator.credentials.create/get` to
  the relay, which acts as the authenticator (ES256, no attestation). The keys themselves are made by the Mac's
  **Secure Enclave** and never leave it: `chrome-relay` asks the chip to create a key or sign. **Every
  sign-in needs your approval**, Touch ID here or Face ID on your paired iPhone, saying who is asking, for which
  site and account: *"chrome-relay is trying to let Claude Code (my-repo) sign in to github.com as
  bot@company.com, from github.com"*. Neither the relay, the vault nor the agent ever holds a private key. If an
  agent signs in to many sites in a row, `chrome-relay passkeys reuse 120` lets one approval cover **that
  agent's** passkey sign-ins for 2 minutes (up to 300 seconds; the prompt says so; other agents still ask). Off by
  default. A site's "Create a passkey" (only right after the agent clicked or typed in the page, and while the
  Mac is unlocked) stores the public key in the vault, in an item of its own (*"GitHub passkey (you) · Chrome
  Relay · Sam's MacBook Pro"*, tagged `chrome-relay-passkey`), in the vault that holds the site's login for that
  username, so a work account's passkey stays in the work account. Chrome Relay never changes your own items: the
  only items it edits are the ones it made. **Each Mac keeps its own:** a passkey's key is in one Mac's Secure
  Enclave, so the item records which Mac (its name, and an ID derived from its hardware UUID). Other Macs sharing
  the vaults neither use nor replace it; give an account a passkey on each Mac you use. The chip's handle for the key stays in Chrome Relay's
  own Keychain item, so only your `chrome-relay`, on this Mac, can use it. Passkeys made by an older Chrome Relay
  keep asking for Touch ID on the Mac itself.
- **Your own 1Password passkeys** (your Google passkey, say) never leave 1Password: no program can read them.
  When a page asks for a passkey the relay doesn't hold, the request goes to 1Password's own prompt in the
  agent's tab, and you approve it. Chrome Relay notifies you, and its toolbar icon shows a red **key** badge.
  Clicking either opens that tab (the only time an agent tab comes forward, and only on your click). The tab
  waits up to 5 minutes, and the agent is told `PASSKEY APPROVAL NEEDED` so it can ask you. This works with no
  vault set up at all.
- **Giving an account a Chrome Relay passkey.** Ask your agent ("add a Chrome Relay passkey to my Google
  account"). It opens the site's passkey settings, you approve the site's "verify it's you" step, and it
  clicks "Create a passkey". From then on that account signs in with only your approval, wherever you are.
  The skill has the steps.
- **Audited.** Every fill, refusal, passkey creation and passkey sign-in is in `chrome-relay audit` (item and
  field names, never values).
- **Signing.** The hardened runtime with library validation on: no library signed by anyone else loads into
  `chrome-relay`, and debuggers and `DYLD_*` injection are blocked. (The 1Password SDK compiles WebAssembly, the
  one exception: `allow-unsigned-executable-memory`.)

**Where the service account tokens live.** In your login Keychain (items *Chrome Relay*, one per account), in
two layers:

1. **Only your signed `chrome-relay` reads the item.** It creates the item itself, so macOS ties it to the
   binary's signature: any other program that asks (`security`, a script, an agent) gets macOS's *"… wants to
   access 'Chrome Relay' in your keychain"* dialog, and nothing unless you allow it. (Most command-line tools,
   Claude Code's and Codex's own logins included, store theirs through Apple's `security` tool, which every
   program on your Mac can run silently. Chrome Relay deliberately doesn't.)
2. **What's in the item is locked to this Mac's Secure Enclave.** Even a program you let through gets only
   ciphertext that opens on this Mac alone. Opening it asks you nothing (after `vault setup`, no Touch ID for
   the token ever); each sign-in is approved instead, as often as you chose. That choice is kept in the same
   item, not in `config.json`, so nothing an agent can edit switches approvals off: change it with
   `chrome-relay vault approve session|sign-in|off`, which asks you first.

### Approving from your iPhone

When you're away from the Mac, a paired iPhone can approve instead: `chrome-relay phone pair` (after Touch ID)
shows a QR code; scan it with the iPhone's Camera and open it in *Chrome Relay Approve* (`ios/`, built and
installed with `./ios/build`, or sent to TestFlight with `./ios/release` so others install it from there). From then on every approval goes to Touch ID here and to the iPhone at once, and
the first answer wins. The iPhone shows the Mac, the agent and what it asks, and approves with Face ID: its key
for that lives in the iPhone's Secure Enclave, and the Mac checks the signature against the key it paired with,
so nothing in between can approve. Requests and answers travel through [ntfy](https://ntfy.sh) (or your own
ntfy server, `--server`), encrypted to the phone.

**How the phone gets woken.** Only Apple can put a notification on a locked iPhone, and it accepts a push for an
app only when it is signed with that app's developer key. So that not every Mac needs the key, a small
[push relay](push-relay/README.md) (a Cloudflare Worker) holds it: the Mac sends the relay the phone's push
address, and the relay asks Apple to show *"An agent is waiting for your approval."* It sees nothing else, keeps
nothing, and can't approve anything. `chrome-relay phone` shows what is in use; your own key (`chrome-relay phone
push --key …`) takes its place if you import one, `CHROME_RELAY_PUSH_RELAY=off` turns the relay off, and without
either the ntfy iPhone app can notify you (`chrome-relay phone` says which topic).

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
CHROME_RELAY_E2E=1 go test ./internal/cli -run TestDo   # `chrome-relay do` against real pages in a throwaway headless Chrome
                                        # (+ CHROME_RELAY_E2E_CLAUDE=1, or CHROME_RELAY_E2E_JEV=1 with TYPESAFE_API_KEY: real deciders)
node --test test/extension-*.test.mjs   # the extension's service worker
node --test test/push-relay.test.mjs    # the push relay (push-relay/)
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

MIT. See [LICENSE](LICENSE). Code adapted from other projects: [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
