---
name: chrome-relay
description: Use the person's own signed-in Chrome from the command line, in background tabs, through Chrome Relay. Load this BEFORE any browser step that needs their logins (internal tools, Google-SSO apps, GitHub, admin consoles, staging environments, OAuth consent screens) or when they say "check it in the browser", "open it logged in", "click through", "use my Chrome". Never drive their Chrome any other way.
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
- Never type credentials or pick accounts yourself. If a command fails with `SIGN-IN NEEDED: ...` (password,
  passkey, 2-step, signed-out account) or you land on any login form: stop, ask the person to sign in in their
  own Chrome window for that profile, and retry after they confirm.
- Popups and "open in new tab" links open as new hidden tabs that belong to you (`agent-browser tab list`).
- Everything you do is in the audit trail (`chrome-relay audit`). Typed text is stored only as its length.
- Never use `--auto-connect`, `--cdp 9222`, `--headed`, Playwright attached to their Chrome, computer use or
  AppleScript against Chrome: those take over their screen.
- Public pages that need no login: a plain headless `agent-browser open <url>` (no `--cdp`) is fine.
