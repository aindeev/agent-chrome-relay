<p align="center"><img src="docs/icon.png" width="96" alt="Chrome Relay icon"></p>

# Chrome Relay

**Website:** https://aindeev.github.io/agent-chrome-relay/


Lets your coding agents (Claude Code, Codex, Cursor, Paseo) use **your everyday Chrome**, with your real
logins, from the command line, **in background tabs**: nothing takes focus, no dialogs, ~0.15s per command.

```bash
agent-browser --cdp "$(chrome-relay url you@company.com)" open https://app.example.com/
agent-browser snapshot -i && agent-browser click @e5 && agent-browser close
```

It's opt-in: nothing installs the extension for you, and it only runs on Macs whose owner set it up.

## How it works

```
agent-browser (CLI) ──ws──▶ relay (127.0.0.1:9333, LaunchAgent) ──ws──▶ Chrome Relay extension ──chrome.debugger──▶ agent tab
```

- **Extension** (`extension/`, MV3, one per Chrome profile you load it in): opens each agent tab **inactive**
  in a collapsed "Agents" tab group and runs DevTools commands in it via `chrome.debugger`. No remote-debugging
  port, so Chrome never shows "Allow remote debugging?". Chrome does show "Chrome Relay started debugging this
  browser" while agents work.
- **Relay** (`relay/relay.mjs`): a DevTools endpoint per agent. Each agent sees and controls only the tabs it
  opened; commands that could raise or focus the browser are swallowed.
- **CLI** (`bin/chrome-relay`): setup, the connection URL, diagnostics, audit trail.

## Setup (once per Mac, ~2 minutes)

Needs macOS, Google Chrome, Node.js 20+ and [`agent-browser`](https://github.com/vercel-labs/agent-browser)
(`npm i -g agent-browser`).

```bash
git clone https://github.com/aindeev/agent-chrome-relay
cd agent-chrome-relay
bin/chrome-relay setup     # secret, background service, identities, `chrome-relay` on PATH, Claude Code skill
```

Then, **in each Chrome profile agents should use**: open `chrome://extensions`, turn on **Developer mode**,
**Load unpacked**, and pick the `extension` folder of your clone. Check with `chrome-relay doctor`.

After `git pull`: `chrome-relay update` (restarts the relay, reloads the extension in every profile).
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

Agents never type credentials. When a site bounces through Google and the right account is **already signed
in**, the extension clicks the account and **Continue/Allow** itself (page scripting, so it works even with
1Password's frame on the page). A password, passkey, 2-step screen or signed-out account stops the hop, and the
agent gets `SIGN-IN NEEDED: ...` and asks you to sign in in your own window.

Which Google account a site uses comes from `~/.chrome-relay/identities.json` (`chrome-relay identities`):
your Chrome profiles (filled in by setup), optional hand-set `sites.rules`, and `sites.learned`, which the
relay fills in after each successful hop. Default: the profile's own account.

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
- **1Password**: Chrome drops an extension's debugger from a page that contains another extension's frame
  (1Password's menu on login forms). The agent's commands wait while the extension re-attaches.
- **Self-cleaning**: tabs of an agent silent for 30 minutes are closed. When the relay restarts, the extension
  closes the agent tabs it opened (tracked by tab id, so your own tab groups are never touched).
- **Real input in background tabs**: focus emulation is on in every agent tab, so clicks and typing land.

## Development

```bash
cd relay && npm install && npm test     # who may connect: secret, allowed apps, Chrome-only extension, audit
```

Logs: `chrome-relay logs` (`~/.chrome-relay/relay.log`). Testing extension changes: edit `extension/`, then
`chrome-relay update`. `CHROME_RELAY_HOME` and `CHROME_RELAY_PORT` override the data folder and port.

## License

MIT. See [LICENSE](LICENSE).
