package relay

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"github.com/aindeev/agent-chrome-relay/internal/vault"
	"html"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The page scripts, byte for byte what the Node relay injected (see shims/).
var (
	//go:embed shims/popup.js
	popupShim string
	//go:embed shims/passkey.js
	passkeyShim string
	//go:embed shims/quiet.js
	quietShim string
	//go:embed shims/focused.js
	focusedExpr string
	//go:embed shims/otpsetup.js
	otpSetupExpr string
)

const binding = "__agentBridgeOpen"

// Commands that could show or focus something, or touch the user's browser outside the agent's tabs.
var swallow = map[string]bool{"Target.activateTarget": true, "Page.bringToFront": true, "Browser.setWindowBounds": true, "Browser.close": true,
	"Browser.crash": true, "Browser.setDownloadBehavior": true, "Browser.setDockTile": true, "Browser.grantPermissions": true,
	"Browser.resetPermissions": true, "Target.createBrowserContext": true, "Target.disposeBrowserContext": true, "Target.setRemoteLocations": true,
	"Browser.setPermission": true}

// Commands that switch something on for a tab; replayed when a tab's debugger session is re-attached.
var setupRe = regexp.MustCompile(`^(Page|Runtime|Network|DOM|CSS|Log|Inspector|Fetch|Security|Accessibility)\.enable$|^Page\.(setLifecycleEventsEnabled|addScriptToEvaluateOnNewDocument|setInterceptFileChooserDialog|setBypassCSP)$|^Runtime\.addBinding$|^Emulation\.|^Network\.(setExtraHTTPHeaders|setCacheDisabled|setUserAgentOverride)$|^Target\.setAutoAttach$`)

// message from an agent (CDP).
type message struct {
	ID        json.RawMessage `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	// sink, when set, gets the reply instead of the agent's WebSocket: the relay's own driver (driver.go) runs
	// commands in an agent's tab through the same path as the agent's, vault references and audit included.
	sink func(key string, v any) `json:"-"`
}

func (m message) params() map[string]any {
	var p map[string]any
	json.Unmarshal(m.Params, &p)
	if p == nil {
		p = map[string]any{}
	}
	return p
}

// extEvent: a DevTools event from one of the extension's tabs.
type extEvent struct {
	TabID     int             `json:"tabId"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
}

type step struct {
	Method string
	Params any
}
type pause struct{ done chan struct{} }
type handoff struct{ rpID, kind string }
type ctxInfo struct {
	frameID   string
	isDefault bool
}
type detach struct{ reason, url string }
type pageCall struct {
	tabID int
	raw   string
	from  callFrom
}
type callFrom struct {
	contextID int
	sessionID string
}

// Agent: one agent connection. All methods run with s.mu held.
type Agent struct {
	s                    *Server
	passkeysUntil        time.Time // one approval covers this agent's passkey sign-ins until then (the reuse window)
	conn                 *conn
	email, app, cwd, sid string
	last                 time.Time
	autoPinned, closed   bool
	autoAttach, discover bool
	tabs                 map[int]bool
	order                []int // tab ids in the order this agent got them
	child                map[string]int
	setup                map[int][]step
	paused               map[int]*pause
	hidden               []string
	lastInput            map[int]time.Time
	contexts             map[int]map[int]ctxInfo
	handoffs             map[int]handoff
	filled               map[int]string // tab -> the vault item the agent last filled there (which account it is signing in as)
	targetURLs, urls     map[int]string
	mainFrames           map[int]string
	popups               map[string]int
	openerOf             map[int][2]int
	orphans              []int
	blocked              string
	pageQ                chan pageCall
	newLogins            map[string]newLogin  // ?new references this agent saved: the "confirm password" field gets the same
	id                   Caller               // who connected: the driver (driver.go) acts for the same agent
	lastTab              int                  // the tab the agent last sent a command to: the driver's default tab
	held                 bool                 // a session the relay holds for `chrome-relay open` (no agent-browser connection)
	dialogArm            map[int]dialogAnswer // tab -> how a held session answers the next confirm or prompt
	dialogSeen           map[int][]string     // tab -> dialogs answered since the driver's last action
	console              map[int][]string     // tab -> what a held session's pages logged and threw (the last 200 lines)
	worlds               map[int]int          // tab -> the driver's own script world in the current document
	refNext              int                  // the next ref number the driver's snapshots hand out (never reused)
}

// newLogin: a password Chrome Relay just made and saved, for the next fields of the same sign-up.
type newLogin struct {
	v  vault.Value
	at time.Time
}

func (a *Agent) init() {
	a.tabs, a.child, a.setup, a.paused = map[int]bool{}, map[string]int{}, map[int][]step{}, map[int]*pause{}
	a.lastInput, a.contexts, a.handoffs, a.filled = map[int]time.Time{}, map[int]map[int]ctxInfo{}, map[int]handoff{}, map[int]string{}
	a.newLogins = map[string]newLogin{}
	a.targetURLs, a.urls, a.mainFrames, a.popups, a.openerOf = map[int]string{}, map[int]string{}, map[int]string{}, map[string]int{}, map[int][2]int{}
	// In order: `window.open(); w.location = url` sends "open" then "nav" back to back, and "nav" needs the tab "open" creates.
	a.pageQ = make(chan pageCall, 1024)
	go func() {
		for c := range a.pageQ {
			a.s.mu.Lock()
			a.onPageCall(c.tabID, c.raw, c.from)
			a.s.mu.Unlock()
		}
	}()
}

func (a *Agent) addTab(t int) {
	if !a.tabs[t] {
		a.order = append(a.order, t)
	}
	a.tabs[t] = true
}
func (a *Agent) dropTab(t int) bool {
	if !a.tabs[t] {
		return false
	}
	delete(a.tabs, t)
	a.order = slices.DeleteFunc(a.order, func(x int) bool { return x == t })
	return true
}
func (a *Agent) tabIDs() []int { return slices.Clone(a.order) }

// p: an agent that named no profile uses whichever is connected, and is pinned to one only when it opens a tab
// there (see Target.createTarget). With no tabs left and that profile gone, the pin is dropped again.
func (a *Agent) p() *profile {
	if a.autoPinned && len(a.tabs) == 0 && a.s.profiles[a.email] == nil {
		a.email, a.autoPinned = "", false
	}
	return a.s.ext(a.email)
}

func (a *Agent) send(v any) {
	if os.Getenv("BRIDGE_TRACE") != "" {
		if m, ok := v.(map[string]any); ok && m["method"] != nil && !regexp.MustCompile(`^(Network|Page\.lifecycle|Runtime\.console|Page\.frame)`).MatchString(fmt.Sprint(m["method"])) {
			p, _ := json.Marshal(m["params"])
			a.s.log("<", m["method"], m["sessionId"], trunc(string(p), 160))
		}
	}
	if !a.conn.open() {
		return
	}
	b, _ := json.Marshal(v)
	a.conn.send([]byte(a.mask(string(b))))
}

// mask hides every vault secret this agent typed, as read back from the page inside JSON.
func (a *Agent) mask(s string) string {
	for _, h := range a.hidden {
		s = strings.ReplaceAll(s, h, "••••••••")
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// hide a secret this agent typed, as it would appear inside JSON, also as HTML writes it (an attribute in a page's
// markup).
func (a *Agent) hide(secret string) {
	if len(secret) < 4 {
		return
	}
	for _, v := range []string{secret, html.EscapeString(secret)} {
		b, _ := json.Marshal(v)
		if h := string(b[1 : len(b)-1]); !slices.Contains(a.hidden, h) {
			a.hidden = append(a.hidden, h)
		}
	}
}

func (a *Agent) reply(m message, key string, v any) {
	if m.sink != nil {
		m.sink(key, v)
		return
	}
	out := map[string]any{"id": m.ID, key: v}
	if m.SessionID != "" {
		out["sessionId"] = m.SessionID
	}
	a.send(out)
}
func (a *Agent) ok(m message, result any) {
	if result == nil {
		result = map[string]any{}
	}
	a.reply(m, "result", result)
}
func (a *Agent) err(m message, msg string) {
	a.reply(m, "error", map[string]any{"code": -32000, "message": msg})
}
func (a *Agent) info(t int) map[string]any {
	u := a.targetURLs[t]
	if u == "" {
		u = "about:blank"
	}
	return map[string]any{"targetId": fmt.Sprintf("tab-%d", t), "type": "page", "title": "", "url": u, "attached": true, "canAccessOpener": false}
}

// tabOf "tab-12" -> 12 (or -1).
func tabOf(v any) int {
	s := fmt.Sprint(v)
	if !strings.HasPrefix(s, "tab-") {
		return -1
	}
	n, err := strconv.Atoi(s[4:])
	if err != nil {
		return -1
	}
	return n
}

func (a *Agent) extMust(p *profile, msg map[string]any) error {
	if r := a.s.extCall(p, msg); r.Error != "" {
		return fmt.Errorf("%s", r.Error)
	}
	return nil
}
func (a *Agent) cdp(p *profile, tab int, method string, params any) extReply {
	return a.s.extCall(p, map[string]any{"op": "cdp", "tabId": tab, "method": method, "params": params})
}
func (a *Agent) cdpSend(p *profile, tab int, method string, params any) {
	a.s.extSend(p, map[string]any{"op": "cdp", "tabId": tab, "method": method, "params": params})
}

func (a *Agent) onMessage(m message) {
	a.last = time.Now()
	if os.Getenv("BRIDGE_TRACE") != "" {
		a.s.log(">", m.Method, m.SessionID, trunc(string(m.Params), 120))
	}
	p := a.p()
	if p == nil {
		who := a.email
		if who == "" {
			who = "any profile"
		}
		a.err(m, "Chrome Relay extension is not connected for "+who)
		return
	}
	if swallow[m.Method] {
		a.ok(m, nil)
		return
	}
	if m.SessionID != "" { // command inside one of this agent's tabs
		tab, child := -1, ""
		if strings.HasPrefix(m.SessionID, "tab-") {
			tab = tabOf(m.SessionID)
		} else if t, ok := a.child[m.SessionID]; ok {
			tab, child = t, m.SessionID
		}
		if tab < 0 || !a.tabs[tab] {
			a.err(m, "unknown session")
			return
		}
		if m.sink == nil {
			a.lastTab = tab
		}
		if child == "" && m.Method == "Runtime.runIfWaitingForDebugger" {
			a.ok(m, nil)
			return
		}
		if w := a.paused[tab]; w != nil { // tab is re-attaching (see onPaused)
			h, waiting := a.handoffs[tab]
			late := false
			a.s.mu.Unlock()
			if waiting { // waiting on the person to approve a passkey in 1Password: say so after 15s instead of hanging
				select {
				case <-w.done:
				case <-time.After(a.s.handoffWait):
					late = true
				}
			} else {
				<-w.done
			}
			a.s.mu.Lock()
			if late && a.paused[tab] == w {
				a.err(m, fmt.Sprintf("PASSKEY APPROVAL NEEDED: the page asked for a passkey for %s, and only 1Password has it. Ask the person at this Mac to click the Chrome Relay icon in Chrome's toolbar (it shows \"key\"), which opens this tab, and approve the passkey in 1Password's prompt. Then continue: this tab works again as soon as they have.", h.rpID))
				return
			}
		}
		if !a.tabs[tab] {
			msg := a.blocked
			if msg == "" {
				msg = "tab was closed"
			}
			a.err(m, msg)
			return
		}
		P := m.params()
		if text, ok := P["text"].(string); ok && m.Method == "Input.insertText" && vaultRef(strings.TrimSpace(text)) {
			if r, save, err := vault.ParseSave(strings.TrimSpace(text)); save {
				a.saveSecret(tab, child, m, strings.TrimSpace(text), r, err)
			} else {
				a.fillSecret(tab, child, m, strings.TrimSpace(text))
			}
			return
		}
		if text, ok := P["text"].(string); ok && m.Method == "Input.insertText" && child == "" && codeLike(text) {
			if el := a.focusedField(p, tab); el != nil && el.Boxes >= len([]rune(text)) {
				a.auditCommand(tab, m, P)
				if r := a.typeText(p, tab, el, text); r.Error != "" {
					a.err(m, r.Error)
				} else {
					a.ok(m, nil)
				}
				return
			}
		}
		if (m.Method == "Input.dispatchMouseEvent" && P["type"] == "mousePressed") || (m.Method == "Input.dispatchKeyEvent" && (P["type"] == "rawKeyDown" || P["type"] == "keyDown")) {
			a.lastInput[tab] = time.Now()
		}
		a.auditCommand(tab, m, P)
		msg := map[string]any{"op": "cdp", "tabId": tab, "method": m.Method, "params": m.Params}
		if child != "" {
			msg["sessionId"] = child
		}
		r := a.s.extCall(p, msg)
		// Reattachment restores the successful active state, not every attempted command. A rejected enable must
		// not poison later setup, and a domain the client disabled must not be silently re-enabled.
		if r.Error == "" && child == "" {
			if setupRe.MatchString(m.Method) {
				a.setup[tab] = append(a.setup[tab], step{m.Method, m.Params})
			} else if strings.HasSuffix(m.Method, ".disable") {
				enable := strings.TrimSuffix(m.Method, ".disable") + ".enable"
				if setupRe.MatchString(enable) {
					a.setup[tab] = slices.DeleteFunc(a.setup[tab], func(s step) bool { return s.Method == enable })
				}
			}
		}
		if r.Error != "" {
			if strings.Contains(r.Error, "Detached while handling command") {
				// Chrome cut the debugger for a moment: another extension (1Password) put its frame in the page. The
				// tab's shim removes it and the extension re-attaches; this command may or may not have run.
				r.Error = "INTERRUPTED: another extension (usually 1Password) briefly put its frame in the page, and Chrome Relay removed it. " +
					"This command may not have run: check the page (and a field's value, if you were typing), then retry it."
				a.s.audit(a, "interrupted", "tab", tab, "method", m.Method)
			}
			a.err(m, r.Error)
		} else {
			a.ok(m, r.Result)
		}
		return
	}
	P := m.params()
	switch m.Method { // browser-level commands, answered for this agent only
	case "Browser.getVersion":
		a.ok(m, map[string]any{"protocolVersion": "1.3", "product": "Chrome/ChromeRelay", "revision": "", "userAgent": "", "jsVersion": ""})
	case "Target.setAutoAttach":
		a.autoAttach = truthy(P["autoAttach"])
		a.ok(m, nil)
		for _, t := range a.tabIDs() {
			a.announce(t)
		}
	case "Target.setDiscoverTargets":
		a.discover = truthy(P["discover"])
		a.ok(m, nil)
	case "Target.getTargets":
		infos := []any{}
		for _, t := range a.tabIDs() {
			infos = append(infos, a.info(t))
		}
		a.ok(m, map[string]any{"targetInfos": infos})
	case "Target.getBrowserContexts":
		a.ok(m, map[string]any{"browserContextIds": []any{}})
	case "Target.getTargetInfo":
		if P["targetId"] == nil || P["targetId"] == "" {
			a.ok(m, map[string]any{"targetInfo": map[string]any{"targetId": "browser", "type": "browser", "title": "", "url": "", "attached": true}})
			return
		}
		t := tabOf(P["targetId"])
		if !a.tabs[t] {
			a.err(m, "No target with given id found")
			return
		}
		a.ok(m, map[string]any{"targetInfo": a.info(t)})
	case "Target.createTarget":
		tc := time.Now()
		r := a.s.extCall(p, map[string]any{"op": "create"})
		createMs := time.Since(tc).Milliseconds()
		if r.Error != "" {
			a.err(m, r.Error)
			return
		}
		var res struct {
			TabID int `json:"tabId"`
		}
		json.Unmarshal(r.Result, &res)
		t := res.TabID
		if a.email == "" { // its tabs live in this profile from now on
			a.email, a.autoPinned = p.prof, true
		}
		tp := time.Now()
		if e := a.prepare(t, nil); e != nil {
			a.err(m, e.Error())
			return
		}
		a.s.log("tab", t, fmt.Sprintf("create %dms, prepare %dms", createMs, time.Since(tp).Milliseconds()))
		a.s.audit(a, "tab open", "tab", t)
		a.ok(m, map[string]any{"targetId": fmt.Sprintf("tab-%d", t)})
		if a.autoAttach {
			a.announce(t)
		}
		if u, _ := P["url"].(string); u != "" && u != "about:blank" {
			a.cdpSend(p, t, "Page.navigate", map[string]any{"url": u})
		}
	case "Target.attachToTarget":
		t := tabOf(P["targetId"])
		if !a.tabs[t] {
			a.err(m, "No target with given id found")
			return
		}
		a.ok(m, map[string]any{"sessionId": fmt.Sprintf("tab-%d", t)})
	case "Target.closeTarget":
		t := tabOf(P["targetId"])
		if !a.tabs[t] {
			a.err(m, "No target with given id found")
			return
		}
		if r := a.s.extCall(p, map[string]any{"op": "close", "tabId": t}); r.Error != "" {
			a.err(m, "could not close the tab: "+r.Error) // still ours: the agent can retry
			return
		}
		a.dropTab(t)
		delete(a.targetURLs, t)
		delete(a.mainFrames, t)
		a.ok(m, map[string]any{"success": true})
		a.send(map[string]any{"method": "Target.detachedFromTarget", "params": map[string]any{"sessionId": fmt.Sprintf("tab-%d", t), "targetId": fmt.Sprintf("tab-%d", t)}})
	default:
		a.err(m, m.Method+" is not available through Chrome Relay")
	}
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	}
	return v != nil
}

// prepare every agent tab: behave as focused (real clicks/typing land in a background tab), route popups
// (window.open, target=_blank links) into new background tabs owned by this agent instead of windows, answer
// passkey requests, and keep password managers out of its forms.
func (a *Agent) prepare(t int, openerID *int) error {
	p := a.p()
	// Creation is asynchronous: the agent may disconnect before Chrome returns the new tab. Its normal close()
	// sweep cannot see that tab yet.
	if a.closed {
		a.s.extCall(p, map[string]any{"op": "close", "tabId": t})
		return fmt.Errorf("agent disconnected while the tab was being created")
	}
	a.addTab(t)
	opener := ""
	if openerID != nil {
		opener = fmt.Sprintf("window.__agentBridgeOpenerId=%d;", *openerID)
	}
	pageScript := strings.TrimSuffix(quietShim, "\n")
	if a.s.passkeySettings().enabled { // else pages' own WebAuthn is left alone
		pageScript = strings.TrimSuffix(passkeyShim, "\n") + pageScript
	}
	steps := []step{{"Emulation.setFocusEmulationEnabled", map[string]any{"enabled": true}}, {"Page.enable", map[string]any{}}, {"Runtime.enable", map[string]any{}},
		{"Runtime.addBinding", map[string]any{"name": binding}},
		{"Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": opener + strings.TrimSuffix(popupShim, "\n"), "runImmediately": true}},
		{"Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": pageScript, "runImmediately": true}}}
	chans := make([]chan extReply, len(steps)) // all at once, like Promise.all
	for i, st := range steps {
		chans[i] = a.s.extSend(p, map[string]any{"op": "cdp", "tabId": t, "method": st.Method, "params": st.Params})
	}
	var failed error
	for _, ch := range chans {
		if r := a.s.await(ch); r.Error != "" && failed == nil {
			failed = fmt.Errorf("%s", r.Error)
		}
	}
	if failed != nil {
		a.dropTab(t)
		a.s.extSend(p, map[string]any{"op": "close", "tabId": t})
		return fmt.Errorf("could not set up the tab: %v", failed)
	}
	a.setup[t] = slices.Clone(steps[1:])
	a.blockPasskeyDialogs(p, t)
	return nil
}

// blockPasskeyDialogs: a passkey request that doesn't come through the relay's page script (one the page makes
// before it, from a frame, or with a saved copy of the browser's own function) would open Chrome's or macOS's "save
// a passkey" dialogs on the person's screen. A virtual authenticator in this tab takes those instead, and never
// confirms, so they time out quietly. Only this tab; best effort: an older Chrome without it still works.
func (a *Agent) blockPasskeyDialogs(p *profile, t int) {
	if r := a.cdp(p, t, "WebAuthn.enable", map[string]any{"enableUI": false}); r.Error != "" {
		a.s.log("tab", t, "can't keep Chrome's passkey dialogs out:", r.Error)
		return
	}
	r := a.cdp(p, t, "WebAuthn.addVirtualAuthenticator", map[string]any{"options": map[string]any{"protocol": "ctap2", "transport": "internal",
		"hasResidentKey": true, "hasUserVerification": true, "isUserVerified": false, "automaticPresenceSimulation": false}})
	if r.Error != "" {
		a.s.log("tab", t, "can't keep Chrome's passkey dialogs out:", r.Error)
	}
}

// onPageCall: a message from page script in an agent tab (window.open, popups' postMessage, passkeys).
func (a *Agent) onPageCall(t int, raw string, from callFrom) {
	if a.closed {
		return
	}
	var c map[string]any
	if json.Unmarshal([]byte(raw), &c) != nil || c == nil {
		return
	}
	// Everything here comes from page script: accept only an integer id and http(s) URLs, never splice page text into code.
	id := 0
	if v, ok := c["id"]; ok {
		f, isNum := v.(float64)
		if !isNum || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
			return
		}
		id = int(f)
	}
	op, _ := c["op"].(string)
	if op == "webauthn" { // may wait on 1Password and Touch ID: don't hold up popups
		a.s.locked(func() { a.onWebAuthn(t, id, c, from) })
		return
	}
	u, _ := c["url"].(string)
	httpURL := regexp.MustCompile(`^https?://`).MatchString(u)
	if op == "nav" && !httpURL {
		return
	}
	if op == "open" && !httpURL && u != "about:blank" { // window.open() with no URL: blank now, `nav` later
		return
	}
	p := a.p()
	key := fmt.Sprintf("%d:%d", t, id)
	switch op {
	case "open":
		r := a.s.extCall(p, map[string]any{"op": "create"})
		if r.Error != "" {
			a.s.log("popup create failed", r.Error)
			return
		}
		var res struct {
			TabID int `json:"tabId"`
		}
		json.Unmarshal(r.Result, &res)
		popup := res.TabID
		a.popups[key] = popup
		a.openerOf[popup] = [2]int{t, id}
		if e := a.prepare(popup, &id); e != nil {
			a.s.log("popup setup failed:", e.Error())
			delete(a.popups, key)
			a.popupGone(popup) // marks the page's window.open() proxy closed, so the opener is not left waiting
			return
		}
		a.reveal(popup, t)
		a.s.log("popup", popup, "for tab", t, u)
		a.s.audit(a, "popup", "tab", popup, "opener", t, "url", cleanURL(u))
		if httpURL {
			a.cdpSend(p, popup, "Page.navigate", map[string]any{"url": u})
		}
	case "closeSelf":
		if _, ok := a.openerOf[t]; ok {
			a.onDetached(t, detach{})
			a.s.extSend(p, map[string]any{"op": "close", "tabId": t})
		}
	case "nav":
		if pt, ok := a.popups[key]; ok {
			a.cdpSend(p, pt, "Page.navigate", map[string]any{"url": u})
		}
	case "close":
		if pt, ok := a.popups[key]; ok {
			a.dropTab(pt)
			a.s.extSend(p, map[string]any{"op": "close", "tabId": pt})
		}
	case "toPopup":
		if pt, ok := a.popups[key]; ok {
			a.deliver(p, c, t, pt, "window.opener")
		}
	case "toOpener":
		if o, ok := a.openerOf[t]; ok {
			a.deliver(p, c, t, o[0], fmt.Sprintf("(window.__agentBridgePopups||{})[%d] || null", o[1]))
		}
	}
}

// originOf a tab: the one the relay saw it navigate to, never what the page claims.
func (a *Agent) originOf(t int) string {
	u, err := url.Parse(a.urls[t])
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "null"
	}
	return u.Scheme + "://" + u.Host
}

// deliver postMessage between an opener and its popup; targetOrigin is enforced against the receiver's page.
func (a *Agent) deliver(p *profile, c map[string]any, from, to int, sourceExpr string) {
	raw := c["targetOrigin"]
	if m, ok := raw.(map[string]any); ok {
		raw = m["targetOrigin"]
	}
	target := "/"
	if raw != nil && raw != "" {
		target = fmt.Sprint(raw)
	}
	var want string
	switch target {
	case "*":
		want = "*"
	case "/":
		want = a.originOf(from)
	default:
		u, err := url.Parse(target)
		if err != nil || u.Scheme == "" || u.Host == "" {
			a.s.log("popup message dropped: bad targetOrigin", target)
			return
		}
		want = u.Scheme + "://" + u.Host
	}
	if want != "*" && want != a.originOf(to) {
		a.s.log("popup message dropped: targetOrigin", want, "!=", a.originOf(to))
		return
	}
	data, _ := json.Marshal(c["data"])
	origin, _ := json.Marshal(a.originOf(from))
	a.cdpSend(p, to, "Runtime.evaluate", map[string]any{"expression": fmt.Sprintf(`(() => { const e = new MessageEvent("message", { data: %s, origin: %s });
        Object.defineProperty(e, "source", { value: %s }); window.dispatchEvent(e); })()`, data, origin, sourceExpr)})
}

func (a *Agent) popupGone(t int) {
	o, ok := a.openerOf[t]
	if !ok {
		return
	}
	delete(a.openerOf, t)
	if p := a.p(); p != nil {
		a.cdpSend(p, o[0], "Runtime.evaluate", map[string]any{"expression": fmt.Sprintf("((window.__agentBridgePopups||{})[%d]||{}).closed = true", o[1])})
	}
}

// adopt a tab an agent page opened despite the shim (the extension caught it).
func (a *Agent) adopt(t, opener int) bool {
	if !a.tabs[opener] || a.tabs[t] {
		return false
	}
	if e := a.prepare(t, nil); e != nil {
		a.s.log("adopt failed:", e.Error())
		return true
	}
	a.reveal(t, opener)
	a.s.log("adopted tab", t, "opened by", opener)
	return true
}

func (a *Agent) reveal(t, opener int) {
	if a.discover {
		info := a.info(t)
		info["attached"] = false
		info["openerId"] = fmt.Sprintf("tab-%d", opener)
		a.send(map[string]any{"method": "Target.targetCreated", "params": map[string]any{"targetInfo": info}})
	}
	if a.autoAttach {
		a.announce(t)
	}
}

func (a *Agent) auditCommand(t int, m message, P map[string]any) {
	u := a.urls[t]
	switch m.Method {
	case "Page.navigate":
		a.s.audit(a, "navigate", "tab", t, "url", cleanURL(fmt.Sprint(P["url"])))
	case "Input.dispatchMouseEvent":
		if P["type"] == "mousePressed" {
			x, _ := P["x"].(float64)
			y, _ := P["y"].(float64)
			a.s.audit(a, "click", "tab", t, "x", int(math.Round(x)), "y", int(math.Round(y)), "url", u)
		}
	case "Input.insertText":
		text, _ := P["text"].(string)
		a.s.audit(a, "type", "tab", t, "chars", len([]rune(text)), "url", u)
	case "Input.dispatchKeyEvent":
		if key, _ := P["key"].(string); P["type"] == "keyDown" && len([]rune(key)) > 1 {
			a.s.audit(a, "key", "tab", t, "key", key)
		}
	case "Runtime.evaluate": // the agent's own scripts (library internals use callFunctionOn and are skipped)
		expr, _ := P["expression"].(string)
		a.s.audit(a, "script", "tab", t, "code", trunc(strings.Join(strings.Fields(expr), " "), 300), "url", u)
	case "Page.captureScreenshot":
		a.s.audit(a, "screenshot", "tab", t, "url", u)
	case "DOM.setFileInputFiles":
		files := []string{}
		if l, ok := P["files"].([]any); ok {
			for _, f := range l {
				files = append(files, filepath.Base(fmt.Sprint(f)))
			}
		}
		a.s.audit(a, "upload", "tab", t, "files", files)
	}
}

func (a *Agent) announce(t int) {
	a.send(map[string]any{"method": "Target.attachedToTarget", "params": map[string]any{"sessionId": fmt.Sprintf("tab-%d", t), "targetInfo": a.info(t), "waitingForDebugger": false}})
}

// onTargetURL: CDP target metadata needs the committed URL, while audit storage keeps its query/fragment redaction.
func (a *Agent) onTargetURL(t int, u string) {
	a.targetURLs[t] = u
	if a.discover {
		a.send(map[string]any{"method": "Target.targetInfoChanged", "params": map[string]any{"targetInfo": a.info(t)}})
	}
	if c := cleanURL(u); a.urls[t] != c {
		a.urls[t] = c
		a.s.audit(a, "page", "tab", t, "url", c)
	}
}

func (a *Agent) onExtEvent(e extEvent) bool {
	if !a.tabs[e.TabID] {
		return false
	}
	var P map[string]any
	json.Unmarshal(e.Params, &P)
	switch e.Method {
	case "Runtime.consoleAPICalled", "Runtime.exceptionThrown":
		if a.held {
			a.logConsole(e.TabID, e.Method, e.Params)
		}
	case "Page.javascriptDialogOpening":
		if a.held { // nobody else is listening for this session: a dialog left open blocks the page
			a.answerDialog(e.TabID, fmt.Sprint(P["type"]), fmt.Sprint(P["message"]))
		}
	case "Page.frameNavigated":
		if f, ok := P["frame"].(map[string]any); ok && (f["parentId"] == nil || f["parentId"] == "") {
			a.mainFrames[e.TabID] = fmt.Sprint(f["id"])
			delete(a.worlds, e.TabID) // a new document: the driver's world went with the old one
			a.onTargetURL(e.TabID, fmt.Sprint(f["url"]))
		}
	case "Page.navigatedWithinDocument":
		if fid, _ := P["frameId"].(string); fid != "" && fid == a.mainFrames[e.TabID] {
			a.onTargetURL(e.TabID, fmt.Sprint(P["url"]))
		}
	}
	// Which script context is the page itself (top frame, main world): only it may ask for a passkey.
	if e.SessionID == "" {
		switch e.Method {
		case "Runtime.executionContextCreated":
			if c, ok := P["context"].(map[string]any); ok {
				aux, _ := c["auxData"].(map[string]any)
				id, _ := c["id"].(float64)
				fid, _ := aux["frameId"].(string)
				def, _ := aux["isDefault"].(bool)
				if a.contexts[e.TabID] == nil {
					a.contexts[e.TabID] = map[int]ctxInfo{}
				}
				a.contexts[e.TabID][int(id)] = ctxInfo{fid, def}
			}
		case "Runtime.executionContextDestroyed":
			id, _ := P["executionContextId"].(float64)
			delete(a.contexts[e.TabID], int(id))
			if a.worlds[e.TabID] == int(id) {
				delete(a.worlds, e.TabID)
			}
		case "Runtime.executionContextsCleared":
			a.contexts[e.TabID] = map[int]ctxInfo{}
			delete(a.worlds, e.TabID)
		}
	}
	if e.Method == "Runtime.bindingCalled" && P["name"] == binding {
		id, _ := P["executionContextId"].(float64)
		payload, _ := P["payload"].(string)
		select {
		case a.pageQ <- pageCall{e.TabID, payload, callFrom{int(id), e.SessionID}}:
		default:
			a.s.log("page call dropped: too many queued")
		}
		return true
	}
	if e.Method == "Target.attachedToTarget" {
		if sid, _ := P["sessionId"].(string); sid != "" {
			a.child[sid] = e.TabID
		}
	}
	if e.Method == "Target.detachedFromTarget" {
		if sid, _ := P["sessionId"].(string); sid != "" {
			delete(a.child, sid)
		}
	}
	sid := e.SessionID
	if sid == "" {
		sid = fmt.Sprintf("tab-%d", e.TabID)
	}
	a.send(map[string]any{"method": e.Method, "params": e.Params, "sessionId": sid})
	return true
}

func (a *Agent) onPaused(t int) bool {
	if !a.tabs[t] || a.paused[t] != nil {
		return false
	}
	a.paused[t] = &pause{done: make(chan struct{})}
	for sid, tt := range a.child {
		if tt == t {
			delete(a.child, sid)
		}
	}
	return true
}

func (a *Agent) onReattached(t int) bool {
	w := a.paused[t]
	if w == nil {
		return false
	}
	p := a.p()
	if _, ok := a.handoffs[t]; ok {
		delete(a.handoffs, t)
		a.s.audit(a, "passkey handoff done", "tab", t, "url", a.urls[t])
		a.s.unalert(t)
	}
	// Same tab, new debugger session: restore what the agent had switched on, then let held commands run.
	err := a.extMust(p, map[string]any{"op": "cdp", "tabId": t, "method": "Emulation.setFocusEmulationEnabled", "params": map[string]any{"enabled": true}})
	for _, st := range a.setup[t] {
		if err != nil {
			break
		}
		err = a.extMust(p, map[string]any{"op": "cdp", "tabId": t, "method": st.Method, "params": st.Params})
	}
	if err == nil {
		a.blockPasskeyDialogs(p, t) // a new debugger session: the old one's authenticator went with it
	}
	if err == nil {
		// A page that loaded while the debugger was off has no passkey script: its passkey requests would reach
		// Chrome's own dialogs, which the person would have to dismiss. Load it again, with the scripts in place.
		r := a.cdp(p, t, "Runtime.evaluate", map[string]any{"expression": "window.__agentBridgeLate === true", "returnByValue": true})
		var late struct {
			Result struct {
				Value bool `json:"value"`
			} `json:"result"`
		}
		if r.Error == "" && json.Unmarshal(r.Result, &late) == nil && late.Result.Value {
			a.s.log("tab", t, "loaded a page while the debugger was off; reloading it so Chrome Relay's page scripts run first")
			err = a.extMust(p, map[string]any{"op": "cdp", "tabId": t, "method": "Page.reload", "params": map[string]any{}})
		}
	}
	if err != nil {
		// All or nothing: without the full setup (focus, popup shim, the agent's own enables) the tab would
		// misbehave silently. Give it up: held commands fail with a clear error and the agent opens a new tab.
		a.s.log("re-attach setup failed for tab", t, "- closing it:", err.Error())
		a.blocked = fmt.Sprintf("the tab could not be restored after Chrome dropped its debugger session (%v); open the page again", err)
		a.onDetached(t, detach{})
		a.s.extSend(p, map[string]any{"op": "close", "tabId": t})
	}
	// whatever happened above, nothing may stay parked on this pause
	if a.paused[t] == w {
		delete(a.paused, t)
	}
	closeOnce(w)
	return true
}

func closeOnce(w *pause) {
	select {
	case <-w.done:
	default:
		close(w.done)
	}
}

func (a *Agent) onDetached(t int, d detach) bool {
	if !a.tabs[t] { // not this agent's tab: leave it alone
		return false
	}
	note, hasNote := a.s.notes[t]
	if d.reason == "blocked" {
		a.orphans = append(a.orphans, t)
		why := "another extension's frame (1Password)"
		if hasNote {
			why = note.NeedsUser
		}
		h, handedOn := a.handoffs[t]
		if handedOn {
			why = "nobody approved the passkey for " + h.rpID + " in 1Password"
			a.s.log("tab", t, "passkey handoff expired: nobody approved the passkey for", h.rpID, "in 1Password")
			a.s.unalert(t)
		}
		a.s.audit(a, "blocked", "tab", t, "url", cleanURL(d.url), "why", why)
		if handedOn {
			a.blocked = fmt.Sprintf("PASSKEY APPROVAL TIMED OUT: the page asked for a passkey for %s that only 1Password has, and nobody approved it in 1Password's prompt within 5 minutes. That tab is no longer yours. When the person is at this Mac, open the page again and ask them to click the Chrome Relay icon in Chrome's toolbar and approve.", h.rpID)
		} else if hasNote {
			site := note.Site
			if site == "" {
				site = "a site"
			}
			a.blocked = fmt.Sprintf("SIGN-IN NEEDED: %s (signing in to %s as %s). Sign in yourself if you can: open the page again and fill the Google login for %s from 1Password (`chrome-relay vault`), with a 2-step code from the vault or from the person's email. If the vault has no such login, ask the person at this Mac to sign in to Google as %s in their own Chrome window for this profile, then retry.", note.NeedsUser, site, note.Account, note.Account, note.Account)
		} else {
			a.blocked = fmt.Sprintf("the page cannot be automated: another extension (usually 1Password) put its frame in it, at %s. If that is a sign-in page, open it again once and retry. If it happens again, ask the person at this Mac to sign in in their own window, then retry.", d.url)
		}
	}
	if w := a.paused[t]; w != nil {
		delete(a.paused, t)
		closeOnce(w)
	}
	a.dropTab(t)
	for _, m := range []map[int]string{a.targetURLs, a.mainFrames} {
		delete(m, t)
	}
	delete(a.contexts, t)
	delete(a.lastInput, t)
	delete(a.handoffs, t)
	delete(a.filled, t)
	a.popupGone(t)
	a.send(map[string]any{"method": "Target.detachedFromTarget", "params": map[string]any{"sessionId": fmt.Sprintf("tab-%d", t), "targetId": fmt.Sprintf("tab-%d", t)}})
	a.send(map[string]any{"method": "Target.targetDestroyed", "params": map[string]any{"targetId": fmt.Sprintf("tab-%d", t)}})
	return true
}

func (a *Agent) close() {
	a.closed = true
	if p := a.p(); p != nil {
		for _, t := range append(a.tabIDs(), a.orphans...) {
			a.s.extSend(p, map[string]any{"op": "close", "tabId": t})
		}
	}
	a.tabs, a.order = map[int]bool{}, nil
	close(a.pageQ)
}
