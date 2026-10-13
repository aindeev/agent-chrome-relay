# Chrome Web Store listing

What to paste into the [developer dashboard](https://chrome.google.com/webstore/devconsole) for Chrome Relay.
The package is `./package-extension`'s zip.

## Store listing

**Description**

Chrome Relay lets the AI coding agents on your Mac (Claude Code, Codex, Cursor and Paseo) use your own Chrome, signed in as you, in background tabs that never interrupt you.

Your agents can check a dashboard, pull a report from a web app, fill in a form in an internal tool or review a staging site. Their tabs open inactive, in a collapsed "Agents" tab group; nothing on your screen moves, and they stop and ask you when a page needs your password.

This extension is one half of Chrome Relay. The other half is a small open-source service you build and run on your own Mac: https://github.com/aindeev/agent-chrome-relay. The extension talks only to that service, over 127.0.0.1, and to nothing else on the internet.

- Agents only see and control the tabs they opened, never yours.
- Only Claude Code, Codex, Cursor and Paseo, holding your Mac's secret, can connect.
- Every action is in an audit trail on your Mac (typed text only as its length).
- Agent tabs close themselves after 30 minutes of silence.

Setup: https://agent-chrome-relay.aindeev.com

**Category**: Developer Tools
**Language**: English
**Store icon**: `extension/icons/icon-128.png`
**Screenshot**: `store/screenshot-1280x800.png`
**Small promo tile**: `store/promo-tile-440x280.png`
**Official URL / Homepage**: https://agent-chrome-relay.aindeev.com
**Support URL**: https://github.com/aindeev/agent-chrome-relay/issues

## Privacy practices

**Single purpose**

Lets the user's local AI coding agents (Claude Code, Codex, Cursor, Paseo) open and operate background tabs in this Chrome profile, through the Chrome Relay service running on the user's own Mac.

**Permission justifications**

- `debugger`: The extension's purpose is to let the user's local coding agents operate web pages. It attaches the Chrome DevTools Protocol to the tabs it opened for agents (only those) and relays the agents' DevTools commands (navigate, click, type, read the page, screenshot) from the Chrome Relay service on the user's Mac.
- `tabs`: To open agents' tabs in the background (inactive), track which tabs are the agents', notice when one closes or a page an agent opened opens a popup, and close agents' tabs when they are done.
- `tabGroups`: To keep agents' tabs in one collapsed "Agents" tab group per window, so they stay out of the user's way.
- `storage`: To remember which tabs it opened for agents across service-worker restarts (so it can close exactly those, and never the user's own), and a random ID for this install.
- `alarms`: To reconnect to the local Chrome Relay service every 30 seconds when it is not running yet or restarted.
- `identity` and `identity.email`: To read this Chrome profile's email address, which the local service uses to tell profiles apart when an agent asks for "the work profile" or "the personal profile". It is sent only to the service on 127.0.0.1.
- `scripting`: When a site in an agent's tab bounces through Google sign-in, the extension runs its own packaged function in that tab to pick the Google account the user configured for the site and press Continue (only for accounts already signed in; it never types credentials).
- `notifications`: To tell the user when an agent's tab needs them (a passkey to approve in their password manager); clicking it brings that tab forward.
- Host permission `<all_urls>`: Agents work on whatever sites the user asks them to (internal tools, dashboards, staging sites), which can't be listed in advance. The extension only acts on tabs it opened for agents.

**Remote code**: No, I am not using remote code. (All code is in the package. The WebSocket to 127.0.0.1 carries DevTools commands, not code the extension runs.)

**Data usage**: check

- Personally identifiable information (the profile's email address, sent only to the service on the user's own Mac)
- Website content (pages in agents' tabs, passed only to the service on the user's own Mac)

and certify all three: not sold to third parties; not used or transferred for purposes unrelated to the single purpose; not used or transferred to determine creditworthiness or for lending.

**Privacy policy URL**: https://agent-chrome-relay.aindeev.com/privacy.html

## Distribution

**Visibility**: Unlisted
**Regions**: All regions
**Pricing**: Free
