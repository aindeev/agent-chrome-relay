package relay

import (
	_ "embed"
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

// agentFor: the caller's agent connection with tabs, the most recently active one. Must hold s.mu.
func (s *Server) agentFor(c Caller) *Agent {
	var best *Agent
	for a := range s.agents {
		if a.conn.open() && len(a.tabs) > 0 && sameAgent(c, a.id) && (best == nil || a.last.After(best.last)) {
			best = a
		}
	}
	return best
}

func (s *Server) serveDriver(w http.ResponseWriter, r *http.Request) {
	js := func(code int, v any) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
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
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	a := s.agentFor(c)
	if a == nil {
		js(http.StatusConflict, map[string]any{"error": "no open tab of yours: open the page first, " +
			"`agent-browser --cdp \"$(chrome-relay url <email>)\" open <url>` (with the same AGENT_BROWSER_SESSION)"})
		return
	}
	a.last = time.Now()
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
		out, err = a.observe(t)
	case "/driver/act":
		out, err = a.act(t, q)
	case "/driver/check":
		out, err = a.check(t, q.Checks)
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
	b, _ := json.Marshal(out)
	masked := a.mask(string(b))
	s.mu.Unlock() // never write to a client with the relay held: a slow reader would stall every agent
	locked = false
	w.Header().Set("content-type", "application/json")
	w.Write([]byte(masked))
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

// eval runs a page script for the driver (not the agent's own command: no audit line). Must hold s.mu.
func (a *Agent) eval(t int, expr string, await bool, v any) error {
	p := a.p()
	if p == nil {
		return errors.New("Chrome Relay extension is not connected")
	}
	r := a.cdp(p, t, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": await})
	if r.Error != "" {
		if strings.Contains(r.Error, "context was destroyed") || strings.Contains(r.Error, "navigated") || strings.Contains(r.Error, "Cannot find context") {
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
	b, _ := json.Marshal(params)
	var failed string
	a.onMessage(message{Method: method, Params: b, SessionID: fmt.Sprintf("tab-%d", t), sink: func(key string, v any) {
		if key == "error" {
			if m, ok := v.(map[string]any); ok {
				failed = fmt.Sprint(m["message"])
			} else {
				failed = fmt.Sprint(v)
			}
		}
	}})
	if failed != "" {
		return errors.New(failed)
	}
	return nil
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

func (a *Agent) observe(t int) (map[string]any, error) {
	var snap map[string]any
	for try := 0; try < 3; try++ {
		err := a.eval(t, driverJS, false, &snap)
		if err == nil && snap != nil {
			if snap["ready"] == "loading" {
				a.waitLoad(t)
				continue
			}
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
	return a.eval(t, fmt.Sprintf("(%s)(%q, %s)", strings.TrimSpace(driveActJS), phase, b), await, v)
}

var keyCodes = map[string]int{"Enter": 13, "Tab": 9, "Escape": 27, "ArrowDown": 40, "ArrowUp": 38, "Backspace": 8, "Space": 32}

// act: check the node is still what was decided on, act, wait for the page to settle, read back the effect, and
// return the page as it is now. A refused or stale action did nothing.
func (a *Agent) act(t int, q driveReq) (map[string]any, error) {
	urlBefore := a.urls[t]
	out := map[string]any{}
	switch q.Kind {
	case "navigate":
		if err := a.input(t, "Page.navigate", map[string]any{"url": q.Text}); err != nil {
			return nil, err
		}
		a.waitLoad(t)
	case "scroll":
		dy := 600
		if q.Text == "up" {
			dy = -600
		}
		if err := a.input(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": 400, "y": 400, "deltaX": 0, "deltaY": dy}); err != nil {
			return nil, err
		}
	case "key":
		code, ok := keyCodes[q.Text]
		if !ok {
			return nil, fmt.Errorf("unknown key %q (Enter, Tab, Escape, ArrowDown, ArrowUp, Backspace, Space)", q.Text)
		}
		down := map[string]any{"type": "keyDown", "key": q.Text, "code": q.Text, "windowsVirtualKeyCode": code, "nativeVirtualKeyCode": code}
		if q.Text == "Enter" {
			down["text"] = "\r"
		}
		if err := a.input(t, "Input.dispatchKeyEvent", down); err != nil {
			return nil, err
		}
		a.input(t, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": q.Text, "code": q.Text, "windowsVirtualKeyCode": code, "nativeVirtualKeyCode": code})
	case "click", "fill", "select":
		var prep struct {
			Stale, Refused string
			X, Y           float64
			Done           bool
		}
		if err := a.actScript(t, "prepare", map[string]any{"node": q.Node, "kind": q.Kind, "option": q.Option, "expect": q.Expect}, false, &prep); err != nil {
			if errors.Is(err, errNavigated) {
				return map[string]any{"stale": "the page changed: observe again"}, nil
			}
			return nil, err
		}
		if prep.Stale != "" || prep.Refused != "" {
			snap, err := a.observe(t)
			return map[string]any{"stale": prep.Stale, "refused": prep.Refused, "page": snap}, err
		}
		if !prep.Done {
			for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
				ev := map[string]any{"type": typ, "x": prep.X, "y": prep.Y}
				if typ != "mouseMoved" {
					ev["button"], ev["clickCount"] = "left", 1
				}
				if err := a.input(t, "Input.dispatchMouseEvent", ev); err != nil {
					return nil, err
				}
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
	snap, err := a.observe(t)
	if err != nil {
		return nil, err
	}
	out["page"] = snap
	out["navigated"] = navigated || (snap["url"] != nil && urlBefore != "" && cleanURL(fmt.Sprint(snap["url"])) != cleanURL(urlBefore))
	what := q.Label
	if what == "" {
		what = q.Text
	}
	if q.Kind == "fill" {
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
