package relay

import (
	"cmp"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The driver: `chrome-relay do` (internal/cli/do.go) decides what to do next; the relay reads the page and acts
// in the calling agent's own tab. Reads are page scripts (driver.js, driveact.js). Input goes through the same
// path as the agent's own commands (onMessage, with a sink for the reply), so vault references, the audit trail
// and everything else the relay does for agent-browser apply unchanged.

var (
	//go:embed shims/driver.js
	driverJS string
	//go:embed shims/driveact.js
	driveActJS string
)

// driveReq is one /driver/* request body.
type driveReq struct {
	Tab    int            `json:"tab"`
	Node   int            `json:"node"`
	Kind   string         `json:"kind"` // act: click, fill, select, key, scroll, navigate
	Text   string         `json:"text"` // fill: the text (or an op:// reference); key: the key; navigate: the URL
	Option string         `json:"option"`
	Expect *driveExpect   `json:"expect"`
	Label  string         `json:"label"` // for the audit trail
	Checks []check        `json:"checks"`
	Event  string         `json:"event"` // note: start or end
	Detail map[string]any `json:"detail"`
	// the driver's own commands (`chrome-relay open`, `screenshot`, ...)
	Profile  string          `json:"profile"`  // open: which Chrome profile, for a new session
	New      bool            `json:"new"`      // open: in a new tab
	All      bool            `json:"all"`      // close: every tab
	Full     bool            `json:"full"`     // screenshot: the whole page
	Selector string          `json:"selector"` // text: of the first match
	Files    []string        `json:"files"`    // act upload
	Limit    int             `json:"limit"`    // observe: how many controls at most (default 300)
	To       int             `json:"to"`       // act drag: the node to drop on
	ToSel    string          `json:"toSelector"`
	What     string          `json:"what"`   // query: html, attr, count, visible, enabled, checked
	Name     string          `json:"name"`   // query attr: which attribute; cdp: the method
	Params   json.RawMessage `json:"params"` // cdp
	Clear    bool            `json:"clear"`  // console: forget what was shown
	Brief    bool            `json:"brief"`  // act: the page's address and title afterwards, not a whole snapshot
}

type driveExpect struct {
	Role  string `json:"role"`
	Label string `json:"label"`
}

// check is one --until condition: text, no-text, url, title, selector, js.
type check struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// callers caches who is on the other end of a driver connection: one lsof/ps lookup per connection, not per step.
var callers = struct {
	sync.Mutex
	m map[string]cachedCaller
}{m: map[string]cachedCaller{}}

type cachedCaller struct {
	c  Caller
	at time.Time
}

func (s *Server) connState(c net.Conn, st http.ConnState) {
	if st == http.StateClosed || st == http.StateHijacked {
		callers.Lock()
		delete(callers.m, c.RemoteAddr().String())
		callers.Unlock()
	}
}

func (s *Server) driverCaller(r *http.Request) Caller {
	callers.Lock()
	cc, ok := callers.m[r.RemoteAddr]
	callers.Unlock()
	if ok && time.Since(cc.at) < 10*time.Minute {
		return cc.c
	}
	_, port, _ := net.SplitHostPort(r.RemoteAddr)
	n, _ := strconv.Atoi(port)
	c := s.caller(n)
	callers.Lock()
	callers.m[r.RemoteAddr] = cachedCaller{c, time.Now()}
	callers.Unlock()
	return c
}

// sameAgent: the driver call comes from the agent that owns the connection: the same Claude Code session, Paseo
// agent or Codex thread, and the same agent-browser session. An app that sets none of those ids (Cursor) must
// name its agent-browser session (AGENT_BROWSER_SESSION) and run in the same folder, so two such agents in one
// repo don't share tabs by accident.
func sameAgent(c, a Caller) bool {
	if c.App != a.App || c.BrowserSession != a.BrowserSession {
		return false
	}
	switch {
	case c.ClaudeSession != "" || a.ClaudeSession != "":
		return c.ClaudeSession == a.ClaudeSession
	case c.PaseoAgent != "" || a.PaseoAgent != "":
		return c.PaseoAgent == a.PaseoAgent
	case c.CodexThread != "" || a.CodexThread != "":
		return c.CodexThread == a.CodexThread
	}
	return c.BrowserSession != "" && c.Cwd != "" && c.Cwd == a.Cwd
}

// agentFor: the caller's session, the most recently active one: an agent-browser connection, or a session the
// relay holds for `chrome-relay open`. prof "" matches any profile. Must hold s.mu.
func (s *Server) agentFor(c Caller, prof string, needTabs bool) *Agent {
	var best *Agent
	for a := range s.agents {
		switch {
		case a.closed || !(a.held || a.conn.open()) || !sameAgent(c, a.id):
		case needTabs && len(a.tabs) == 0:
		case prof != "" && a.email != prof: // a session not pinned to a profile yet may be in any of them
		case best == nil || a.last.After(best.last):
			best = a
		}
	}
	return best
}

// hold starts a session the relay keeps for the caller: its tabs live until `chrome-relay close`, or 30 idle
// minutes. Must hold s.mu.
func (s *Server) hold(c Caller, prof string) *Agent {
	b := make([]byte, 3)
	rand.Read(b)
	a := &Agent{s: s, conn: &conn{}, email: prof, app: c.App, cwd: c.Cwd, sid: hex.EncodeToString(b), last: time.Now(), id: c, held: true}
	a.init()
	s.agents[a] = true
	s.audit(a, "session start", "pid", c.PID, "claudeSession", c.ClaudeSession, "paseoAgent", c.PaseoAgent, "codexThread", c.CodexThread,
		"agentBrowserSession", c.BrowserSession, "cwd", c.Cwd, "driver", true)
	s.log("driver session for", cmp.Or(prof, "(default)"), "via", c.App)
	return a
}

// endHeld closes a held session and its tabs. Must hold s.mu.
func (s *Server) endHeld(a *Agent) {
	if a.closed {
		return
	}
	s.audit(a, "session end", "tabs", len(a.tabs))
	delete(s.agents, a)
	a.close()
}

func (s *Server) serveDriver(w http.ResponseWriter, r *http.Request) {
	var a *Agent
	locked := false
	// every answer is masked (a page's error can quote what it holds), and written with the relay released: a slow
	// reader would stall every agent
	js := func(code int, v any) {
		b, _ := json.Marshal(v)
		out := string(b)
		if a != nil {
			out = a.mask(out)
		}
		if locked {
			s.mu.Unlock()
			locked = false
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(code)
		w.Write([]byte(out + "\n"))
	}
	c := s.driverCaller(r)
	if c.App == "" {
		js(http.StatusForbidden, map[string]any{"error": "Chrome Relay only accepts Claude Code, Codex, Cursor and Paseo"})
		return
	}
	var q driveReq
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&q); err != nil {
			js(http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
			return
		}
	}
	s.mu.Lock()
	locked = true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	if r.URL.Path == "/driver/open" {
		if !sameAgent(c, c) {
			// it could never find this session again: every later command would open another
			js(http.StatusConflict, map[string]any{"error": "Chrome Relay can't tell your agent apart from others in this folder: " +
				"set CHROME_RELAY_SESSION=<a name for this agent> and run the commands again"})
			return
		}
		if a = s.agentFor(c, q.Profile, false); a == nil {
			if s.ext(q.Profile) == nil {
				js(http.StatusConflict, map[string]any{"error": "Chrome Relay extension is not connected for " + cmp.Or(q.Profile, "any profile") + ": `chrome-relay doctor`"})
				return
			}
			a = s.hold(c, q.Profile)
		}
	} else if a = s.agentFor(c, "", true); a == nil {
		js(http.StatusConflict, map[string]any{"error": "no open tab of yours: open the page first, `chrome-relay open <url>` " +
			"(with the same CHROME_RELAY_SESSION, if you set one)"})
		return
	}
	a.last = time.Now()
	if r.URL.Path == "/driver/open" {
		out, err := a.open(q)
		if err != nil {
			if a.held && len(a.tabs) == 0 {
				s.endHeld(a)
			}
			js(http.StatusOK, map[string]any{"error": err.Error()})
			return
		}
		js(http.StatusOK, out)
		return
	}
	if r.URL.Path == "/driver/tabs" {
		tabs := []map[string]any{}
		for _, t := range a.tabIDs() {
			tabs = append(tabs, map[string]any{"tab": t, "url": a.urls[t], "current": t == a.currentTab()})
		}
		js(http.StatusOK, map[string]any{"tabs": tabs, "profile": a.email})
		return
	}
	t := q.Tab
	if t == 0 {
		t = a.currentTab()
	}
	if !a.tabs[t] {
		js(http.StatusNotFound, map[string]any{"error": fmt.Sprintf("tab %d is not one of yours", t)})
		return
	}
	var out map[string]any
	var err error
	switch r.URL.Path {
	case "/driver/observe":
		out, err = a.observe(t, q.Limit)
	case "/driver/query":
		var res struct {
			Value        any
			Error, Stale string
		}
		if err = a.actScript(t, "query", map[string]any{"node": q.Node, "selector": q.Selector, "what": q.What, "name": q.Name}, false, &res); err == nil && res.Error != "" {
			err = errors.New(res.Error)
		}
		if errors.Is(err, errNavigated) || res.Stale != "" {
			err = errors.New("the page changed: take a new snapshot")
		}
		s.audit(a, "do read", "tab", t, "what", q.What, "selector", trunc(q.Selector, 120), "url", a.urls[t])
		out = map[string]any{"value": res.Value}
	case "/driver/console":
		out = map[string]any{"lines": append([]string{}, a.console[t]...)}
		if q.Clear {
			delete(a.console, t)
		}
	case "/driver/cdp":
		// any other DevTools command, through the same gate as an agent's own (what could show or focus the browser
		// is still swallowed, and it's audited the same way)
		var params map[string]any
		if len(q.Params) > 0 {
			if err = json.Unmarshal(q.Params, &params); err != nil {
				break
			}
		}
		var raw json.RawMessage
		if raw, err = a.call(t, q.Name, params); err == nil {
			out = map[string]any{"result": raw}
		}
	case "/driver/act":
		out, err = a.act(t, q)
	case "/driver/check":
		out, err = a.check(t, q.Checks)
	case "/driver/close":
		out, err = a.closeTabs(t, q.All)
	case "/driver/dialog":
		if a.dialogArm == nil {
			a.dialogArm = map[int]dialogAnswer{}
		}
		a.dialogArm[t] = dialogAnswer{q.Kind == "accept", q.Text}
		out = map[string]any{}
	case "/driver/tab":
		a.lastTab = t
		out = map[string]any{"url": a.urls[t]}
	case "/driver/eval":
		out, err = a.evalFor(t, q.Text)
	case "/driver/screenshot":
		out, err = a.screenshot(t, q.Full)
	case "/driver/text":
		var res struct{ Text, Error string }
		if err = a.actScript(t, "text", map[string]any{"node": q.Node, "selector": q.Selector}, false, &res); err == nil && res.Error != "" {
			err = errors.New(res.Error)
		}
		s.audit(a, "do read", "tab", t, "selector", trunc(q.Selector, 120), "url", a.urls[t])
		out = map[string]any{"text": res.Text}
	case "/driver/note":
		kv := []any{"tab", t}
		for _, k := range []string{"goal", "decider", "status", "steps", "data", "why", "flow"} {
			if v, ok := q.Detail[k]; ok {
				kv = append(kv, k, v)
			}
		}
		s.audit(a, "do "+q.Event, kv...)
		out = map[string]any{}
	default:
		js(http.StatusNotFound, map[string]any{"error": "unknown driver call"})
		return
	}
	if err != nil {
		js(http.StatusOK, map[string]any{"error": err.Error(), "tab": t})
		return
	}
	out["tab"] = t
	js(http.StatusOK, out)
}

// dialogAnswer: what `chrome-relay dialog accept|dismiss` set for the next confirm or prompt.
type dialogAnswer struct {
	accept bool
	text   string
}

// answerDialog: a page's alert, confirm or prompt in a session the relay holds. Alerts (and "leave this page?") are
// accepted, as agent-browser does. A confirm or prompt is dismissed, the safe answer, unless the agent said what
// to answer beforehand. Either way the page never waits. Must hold s.mu.
func (a *Agent) answerDialog(t int, typ, msg string) {
	accept, text := typ == "alert" || typ == "beforeunload", ""
	if arm, ok := a.dialogArm[t]; ok && (typ == "confirm" || typ == "prompt") {
		accept, text = arm.accept, arm.text
		delete(a.dialogArm, t)
	}
	if p := a.p(); p != nil {
		a.cdpSend(p, t, "Page.handleJavaScriptDialog", map[string]any{"accept": accept, "promptText": text})
	}
	did := map[bool]string{true: "accepted", false: "dismissed"}[accept]
	if a.dialogSeen == nil {
		a.dialogSeen = map[int][]string{}
	}
	a.dialogSeen[t] = append(a.dialogSeen[t], fmt.Sprintf("%s %q: %s", typ, trunc(msg, 200), did))
	a.s.audit(a, "dialog", "tab", t, "kind", typ, "why", did, "url", a.urls[t])
}

// logConsole keeps what a held session's page logs (console.*) and throws, for `chrome-relay console` and `errors`.
func (a *Agent) logConsole(t int, method string, params json.RawMessage) {
	line := ""
	if method == "Runtime.exceptionThrown" {
		var e struct {
			ExceptionDetails struct {
				Text      string
				URL       string
				Exception struct{ Description string }
			}
		}
		json.Unmarshal(params, &e)
		line = "error: " + strings.TrimSpace(e.ExceptionDetails.Text+" "+e.ExceptionDetails.Exception.Description)
	} else {
		var e struct {
			Type string
			Args []struct {
				Type, Description string
				Value             json.RawMessage
			}
		}
		json.Unmarshal(params, &e)
		parts := []string{}
		for _, arg := range e.Args {
			var str string
			switch {
			case json.Unmarshal(arg.Value, &str) == nil && len(arg.Value) > 0:
				parts = append(parts, str)
			case len(arg.Value) > 0:
				parts = append(parts, string(arg.Value))
			default:
				parts = append(parts, arg.Description)
			}
		}
		line = e.Type + ": " + strings.Join(parts, " ")
	}
	if a.console == nil {
		a.console = map[int][]string{}
	}
	l := append(a.console[t], time.Now().Format("15:04:05")+" "+trunc(line, 2000))
	if len(l) > 200 {
		l = l[len(l)-200:]
	}
	a.console[t] = l
}

// currentTab: the tab the agent last used, else its newest.
func (a *Agent) currentTab() int {
	if a.tabs[a.lastTab] {
		return a.lastTab
	}
	if ids := a.tabIDs(); len(ids) > 0 {
		return ids[len(ids)-1]
	}
	return 0
}

var errNavigated = errors.New("the page navigated")

// eval runs a script in the page's own world for the driver (not the agent's own command: no audit line). Must
// hold s.mu.
func (a *Agent) eval(t int, expr string, await bool, v any) error {
	return a.evalIn(t, 0, expr, await, v)
}

// evalDriver runs one of the driver's own scripts (snapshot, act) in a script world of its own: the page's scripts
// can't see or replace what it relies on (its node table, getBoundingClientRect, elementFromPoint), so they can't
// steer where its clicks land. Must hold s.mu.
func (a *Agent) evalDriver(t int, expr string, await bool, v any) error {
	ctx, err := a.world(t)
	if err != nil {
		return err
	}
	return a.evalIn(t, ctx, expr, await, v)
}

// world: the driver's script world in the tab's current document, made the first time it's needed. Must hold s.mu.
func (a *Agent) world(t int) (int, error) {
	if id := a.worlds[t]; id != 0 {
		return id, nil
	}
	p := a.p()
	if p == nil {
		return 0, errors.New("Chrome Relay extension is not connected")
	}
	r := a.cdp(p, t, "Page.getFrameTree", map[string]any{})
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if r.Error != "" || json.Unmarshal(r.Result, &tree) != nil || tree.FrameTree.Frame.ID == "" {
		return 0, fmt.Errorf("could not reach the page: %s", r.Error)
	}
	r = a.cdp(p, t, "Page.createIsolatedWorld", map[string]any{"frameId": tree.FrameTree.Frame.ID, "worldName": "chrome-relay-driver"})
	var w struct {
		ID int `json:"executionContextId"`
	}
	if r.Error != "" || json.Unmarshal(r.Result, &w) != nil || w.ID == 0 {
		return 0, errNavigated
	}
	if a.worlds == nil {
		a.worlds = map[int]int{}
	}
	a.worlds[t] = w.ID
	return w.ID, nil
}

func (a *Agent) evalIn(t, ctx int, expr string, await bool, v any) error {
	p := a.p()
	if p == nil {
		return errors.New("Chrome Relay extension is not connected")
	}
	params := map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": await}
	if ctx != 0 {
		params["contextId"] = ctx
	}
	r := a.cdp(p, t, "Runtime.evaluate", params)
	if r.Error != "" {
		if strings.Contains(r.Error, "context was destroyed") || strings.Contains(r.Error, "navigated") || strings.Contains(r.Error, "Cannot find context") {
			if ctx != 0 && a.worlds[t] == ctx {
				delete(a.worlds, t)
			}
			return errNavigated
		}
		return errors.New(r.Error)
	}
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		return err
	}
	if res.Exception != nil {
		d := res.Exception.Exception.Description
		if strings.Contains(d, "context was destroyed") || strings.Contains(d, "navigated") {
			return errNavigated
		}
		return fmt.Errorf("page script failed: %s", trunc(strings.TrimSpace(res.Exception.Text+" "+d), 200))
	}
	if v == nil || len(res.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(res.Result.Value, v)
}

// input sends one CDP input command the way the agent's own would go (audit, vault references). Must hold s.mu.
func (a *Agent) input(t int, method string, params map[string]any) error {
	_, err := a.call(t, method, params)
	return err
}

// call runs one DevTools command the way the agent's own would go, and returns its result; t 0 is a browser-level
// command (Target.createTarget). Must hold s.mu.
func (a *Agent) call(t int, method string, params map[string]any) (json.RawMessage, error) {
	b, _ := json.Marshal(params)
	var failed string
	var result json.RawMessage
	m := message{Method: method, Params: b, sink: func(key string, v any) {
		if key == "error" {
			if m, ok := v.(map[string]any); ok {
				failed = fmt.Sprint(m["message"])
			} else {
				failed = fmt.Sprint(v)
			}
			return
		}
		result, _ = json.Marshal(v)
	}}
	if t != 0 {
		m.SessionID = fmt.Sprintf("tab-%d", t)
	}
	a.onMessage(m)
	if failed != "" {
		return nil, errors.New(failed)
	}
	return result, nil
}

// open: a URL in the session's current tab, or in a new one (the first is always new).
func (a *Agent) open(q driveReq) (map[string]any, error) {
	t := a.currentTab()
	if q.New || !a.tabs[t] {
		raw, err := a.call(0, "Target.createTarget", map[string]any{"url": "about:blank"})
		if err != nil {
			return nil, err
		}
		var res struct {
			TargetID string `json:"targetId"`
		}
		json.Unmarshal(raw, &res)
		if t = tabOf(res.TargetID); !a.tabs[t] {
			return nil, errors.New("Chrome did not open a tab")
		}
	}
	a.lastTab = t
	out := map[string]any{"tab": t, "profile": a.email}
	if q.Text != "" {
		res, err := a.act(t, driveReq{Kind: "navigate", Text: q.Text})
		if err != nil {
			return nil, err
		}
		out["page"] = res["page"]
	}
	return out, nil
}

// closeTabs: one tab, or all; a held session with none left ends.
func (a *Agent) closeTabs(t int, all bool) (map[string]any, error) {
	ids := []int{t}
	if all {
		ids = a.tabIDs()
	}
	for _, id := range ids {
		if _, err := a.call(0, "Target.closeTarget", map[string]any{"targetId": fmt.Sprintf("tab-%d", id)}); err != nil {
			return nil, err
		}
	}
	left := len(a.tabs)
	if a.held && left == 0 {
		a.s.endHeld(a)
	}
	return map[string]any{"closed": len(ids), "left": left}, nil
}

// evalFor runs the agent's own script in the page (audited as a script, like agent-browser's eval).
func (a *Agent) evalFor(t int, expr string) (map[string]any, error) {
	raw, err := a.call(t, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true})
	if err != nil {
		return nil, err
	}
	var res struct {
		Result struct {
			Type, Description string
			Value             json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	if res.Exception != nil {
		return nil, fmt.Errorf("%s", trunc(strings.TrimSpace(res.Exception.Text+" "+res.Exception.Exception.Description), 500))
	}
	if len(res.Result.Value) == 0 {
		return map[string]any{"value": nil, "type": res.Result.Type, "description": res.Result.Description}, nil
	}
	return map[string]any{"value": res.Result.Value}, nil
}

// screenshot of the tab as PNG (base64): what's in view, or the whole page.
func (a *Agent) screenshot(t int, full bool) (map[string]any, error) {
	params := map[string]any{"format": "png"}
	if full {
		var size struct{ W, H float64 }
		if err := a.eval(t, "({w: Math.max(document.documentElement.scrollWidth, innerWidth), h: Math.max(document.documentElement.scrollHeight, innerHeight)})", false, &size); err == nil && size.W > 0 {
			params["captureBeyondViewport"] = true
			params["clip"] = map[string]any{"x": 0, "y": 0, "width": size.W, "height": min(size.H, 16000), "scale": 1}
		}
	}
	raw, err := a.call(t, "Page.captureScreenshot", params)
	if err != nil {
		return nil, err
	}
	var res struct {
		Data string `json:"data"`
	}
	if json.Unmarshal(raw, &res) != nil || res.Data == "" {
		return nil, errors.New("Chrome returned no image")
	}
	return map[string]any{"png": res.Data}, nil
}

// waitLoad: after a navigation, until the new document is complete (at most 15 s).
func (a *Agent) waitLoad(t int) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		if a.eval(t, "document.readyState", false, &state) == nil && state == "complete" {
			return
		}
		a.s.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		a.s.mu.Lock()
		if !a.tabs[t] {
			return
		}
	}
}

func (a *Agent) observe(t int, limit ...int) (map[string]any, error) {
	var snap map[string]any
	n := 0
	if len(limit) > 0 && limit[0] > 0 {
		n = limit[0]
	}
	script := fmt.Sprintf("window.__crDriveLimit = %d; window.__crDriveStart = %d;\n", n, max(a.refNext, 1)) + driverJS
	for try := 0; try < 3; try++ {
		err := a.evalDriver(t, script, false, &snap)
		if err == nil && snap != nil {
			if snap["ready"] == "loading" {
				a.waitLoad(t)
				continue
			}
			if next, ok := snap["next"].(float64); ok && int(next) > a.refNext {
				a.refNext = int(next)
			}
			delete(snap, "next")
			return snap, nil
		}
		if err != nil && !errors.Is(err, errNavigated) {
			return nil, err
		}
		a.waitLoad(t)
	}
	if snap == nil {
		return nil, errors.New("the page did not settle")
	}
	return snap, nil
}

func (a *Agent) actScript(t int, phase string, q any, await bool, v any) error {
	b, _ := json.Marshal(q)
	return a.evalDriver(t, fmt.Sprintf("(%s)(%q, %s)", strings.TrimSpace(driveActJS), phase, b), await, v)
}

var keyCodes = map[string]int{"Enter": 13, "Tab": 9, "Escape": 27, "ArrowDown": 40, "ArrowUp": 38, "ArrowLeft": 37, "ArrowRight": 39,
	"Backspace": 8, "Delete": 46, "Space": 32, "Home": 36, "End": 35, "PageUp": 33, "PageDown": 34}

// act: check the node is still what was decided on, act, wait for the page to settle, read back the effect, and
// return the page as it is now. A refused or stale action did nothing.
func (a *Agent) act(t int, q driveReq) (map[string]any, error) {
	urlBefore := a.urls[t]
	out := map[string]any{}
	delete(a.dialogSeen, t)
	switch q.Kind {
	case "navigate":
		if err := a.input(t, "Page.navigate", map[string]any{"url": q.Text}); err != nil {
			return nil, err
		}
		a.waitLoad(t)
	case "scroll":
		dy := 600
		if n, err := strconv.Atoi(q.Option); err == nil && n > 0 {
			dy = n
		}
		if q.Text == "up" {
			dy = -dy
		}
		if err := a.input(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": 400, "y": 400, "deltaX": 0, "deltaY": dy}); err != nil {
			return nil, err
		}
	case "back":
		if err := a.eval(t, "history.back()", false, nil); err != nil && !errors.Is(err, errNavigated) {
			return nil, err
		}
		a.s.mu.Unlock()
		time.Sleep(150 * time.Millisecond) // the navigation starts after the script returns
		a.s.mu.Lock()
		a.waitLoad(t)
	case "reload":
		if err := a.input(t, "Page.reload", map[string]any{}); err != nil {
			return nil, err
		}
		a.waitLoad(t)
	case "key":
		code, ok := keyCodes[q.Text]
		if !ok {
			return nil, fmt.Errorf("unknown key %q (Enter, Tab, Escape, Space, Backspace, Delete, Arrow*, Home, End, PageUp, PageDown)", q.Text)
		}
		key := q.Text
		down := map[string]any{"type": "keyDown", "key": key, "code": q.Text, "windowsVirtualKeyCode": code, "nativeVirtualKeyCode": code}
		switch q.Text {
		case "Enter":
			down["text"] = "\r"
		case "Space": // the key is " " (what pages and Blink check for), its code "Space"
			key, down["key"], down["text"] = " ", " ", " "
		}
		if err := a.input(t, "Input.dispatchKeyEvent", down); err != nil {
			return nil, err
		}
		a.input(t, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": key, "code": q.Text, "windowsVirtualKeyCode": code, "nativeVirtualKeyCode": code})
	case "drag":
		var prep struct {
			Refused, Stale string
			X, Y, TX, TY   float64
			Node           int
		}
		if err := a.actScript(t, "drag", map[string]any{"node": q.Node, "selector": q.Selector, "toNode": q.To, "toSelector": q.ToSel}, false, &prep); err != nil {
			return nil, err
		}
		if prep.Stale != "" || prep.Refused != "" {
			snap, err := a.observe(t)
			return map[string]any{"stale": prep.Stale, "refused": prep.Refused, "page": snap}, err
		}
		q.Node = prep.Node
		// press, move across with the button held, release: what a hand does, and what agent-browser sends
		a.input(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": prep.X, "y": prep.Y})
		if err := a.input(t, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": prep.X, "y": prep.Y, "button": "left", "buttons": 1, "clickCount": 1}); err != nil {
			return nil, err
		}
		for i := 1; i <= 10; i++ {
			f := float64(i) / 10
			a.input(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": prep.X + (prep.TX-prep.X)*f, "y": prep.Y + (prep.TY-prep.Y)*f, "button": "left", "buttons": 1})
			a.s.mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			a.s.mu.Lock()
		}
		if err := a.input(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": prep.TX, "y": prep.TY, "button": "left", "buttons": 0, "clickCount": 1}); err != nil {
			return nil, err
		}
	case "click", "dblclick", "fill", "type", "select", "hover", "upload":
		var prep struct {
			Stale, Refused string
			X, Y           float64
			Done           bool
			Node           int
		}
		if err := a.actScript(t, "prepare", map[string]any{"node": q.Node, "selector": q.Selector, "kind": q.Kind, "option": q.Option, "expect": q.Expect}, false, &prep); err != nil {
			if errors.Is(err, errNavigated) {
				return map[string]any{"stale": "the page changed: observe again"}, nil
			}
			return nil, err
		}
		if prep.Stale != "" || prep.Refused != "" {
			snap, err := a.observe(t)
			return map[string]any{"stale": prep.Stale, "refused": prep.Refused, "page": snap}, err
		}
		if prep.Node != 0 {
			q.Node = prep.Node // a selector's element, for the read-back below
		}
		if q.Kind == "upload" {
			// the file field itself, by reference: files are set on it, nothing is clicked
			p := a.p()
			if p == nil {
				return nil, errors.New("Chrome Relay extension is not connected")
			}
			ctx, err := a.world(t)
			if err != nil {
				return nil, err
			}
			r := a.cdp(p, t, "Runtime.evaluate", map[string]any{"expression": fmt.Sprintf("window.__crDrive.nodes.get(%d)", q.Node), "contextId": ctx})
			var obj struct {
				Result struct {
					ObjectID string `json:"objectId"`
				} `json:"result"`
			}
			if r.Error != "" || json.Unmarshal(r.Result, &obj) != nil || obj.Result.ObjectID == "" {
				return nil, errors.New("could not reach the file field")
			}
			if err := a.input(t, "DOM.setFileInputFiles", map[string]any{"files": q.Files, "objectId": obj.Result.ObjectID}); err != nil {
				return nil, err
			}
		}
		if q.Kind == "hover" {
			if err := a.input(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": prep.X, "y": prep.Y}); err != nil {
				return nil, err
			}
		} else if !prep.Done {
			events := []string{"mouseMoved", "mousePressed", "mouseReleased"}
			if q.Kind == "dblclick" {
				events = append(events, "mousePressed", "mouseReleased")
			}
			for i, typ := range events {
				ev := map[string]any{"type": typ, "x": prep.X, "y": prep.Y}
				if typ != "mouseMoved" {
					ev["button"], ev["clickCount"] = "left", 1+(i-1)/2
				}
				if err := a.input(t, "Input.dispatchMouseEvent", ev); err != nil {
					return nil, err
				}
			}
		}
		if q.Kind == "type" { // add to what's there, at the end
			a.input(t, "Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": "End", "code": "End", "windowsVirtualKeyCode": 35, "commands": []string{"moveToEndOfDocument"}})
			a.input(t, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "End", "code": "End", "windowsVirtualKeyCode": 35})
			if err := a.input(t, "Input.insertText", map[string]any{"text": q.Text}); err != nil {
				return nil, err
			}
		}
		if q.Kind == "fill" {
			mod := 4 // ⌘ on a Mac: select what's there, so the text replaces it
			a.input(t, "Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": "a", "code": "KeyA", "modifiers": mod, "windowsVirtualKeyCode": 65, "commands": []string{"selectAll"}})
			a.input(t, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "a", "code": "KeyA", "modifiers": mod, "windowsVirtualKeyCode": 65})
			text := q.Text
			if text == "" {
				a.input(t, "Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": "Backspace", "code": "Backspace", "windowsVirtualKeyCode": 8})
				a.input(t, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "Backspace", "code": "Backspace", "windowsVirtualKeyCode": 8})
			} else if err := a.input(t, "Input.insertText", map[string]any{"text": text}); err != nil {
				return nil, err // a refused 1Password reference says why
			}
		}
	default:
		return nil, fmt.Errorf("unknown action %q", q.Kind)
	}
	navigated := false
	if err := a.actScript(t, "settle", map[string]any{"node": q.Node, "kind": q.Kind}, true, nil); errors.Is(err, errNavigated) {
		navigated = true
	}
	if q.Node != 0 && !navigated {
		var rb map[string]any
		if err := a.actScript(t, "readback", map[string]any{"node": q.Node}, false, &rb); err == nil {
			out["readback"] = rb
		} else if errors.Is(err, errNavigated) {
			navigated = true
		}
	}
	if navigated {
		a.waitLoad(t)
	}
	var snap map[string]any
	if q.Brief {
		// the key as a snapshot now would have it: a new address or a new document is a changed page
		const brief = `({url: location.href, title: document.title, key: location.href + "|" + performance.timeOrigin})`
		err := a.eval(t, brief, false, &snap)
		if errors.Is(err, errNavigated) {
			a.waitLoad(t)
			err = a.eval(t, brief, false, &snap)
		}
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		if snap, err = a.observe(t); err != nil {
			return nil, err
		}
	}
	out["page"] = snap
	if d := a.dialogSeen[t]; len(d) > 0 {
		out["dialogs"] = d
	}
	out["navigated"] = navigated || (snap["url"] != nil && urlBefore != "" && cleanURL(fmt.Sprint(snap["url"])) != cleanURL(urlBefore))
	what := q.Label
	if what == "" {
		what = q.Text
	}
	if q.Kind == "fill" || q.Kind == "type" {
		what = q.Label // never the typed text
	}
	if q.Kind == "navigate" {
		what = cleanURL(q.Text) // no query string, as for the agent's own navigations
	}
	a.s.audit(a, "do "+q.Kind, "tab", t, "label", trunc(what, 120), "url", a.urls[t])
	return out, nil
}

func (a *Agent) check(t int, checks []check) (map[string]any, error) {
	met := make([]bool, len(checks))
	var page []check
	idx := []int{}
	errs := []string{}
	for i, c := range checks {
		if c.Kind == "js" {
			a.s.audit(a, "script", "tab", t, "code", trunc(strings.Join(strings.Fields(c.Value), " "), 300), "url", a.urls[t])
			var v bool
			err := a.eval(t, "(async () => !!(await ("+c.Value+")))()", true, &v)
			if err != nil && !errors.Is(err, errNavigated) {
				errs = append(errs, "js:"+c.Value+": "+err.Error())
			}
			met[i] = err == nil && v
			continue
		}
		page = append(page, c)
		idx = append(idx, i)
	}
	if len(page) > 0 {
		var res []bool
		if err := a.actScript(t, "check", map[string]any{"node": 0, "checks": page}, false, &res); err != nil {
			if !errors.Is(err, errNavigated) {
				return nil, err
			}
		}
		for j, i := range idx {
			met[i] = j < len(res) && res[j]
		}
	}
	all := true
	for _, m := range met {
		all = all && m
	}
	return map[string]any{"met": met, "all": all, "errors": errs}, nil
}
