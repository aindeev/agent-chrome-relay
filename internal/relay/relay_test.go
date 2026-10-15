package relay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	op "github.com/1password/onepassword-sdk-go"
	"github.com/gorilla/websocket"

	"github.com/aindeev/agent-chrome-relay/internal/phone"
	"github.com/aindeev/agent-chrome-relay/internal/vault"
)

const extID = "abcdefghijklmnopabcdefghijklmnop"

// harness: a real relay on a test server; who connects is decided by the test (no lsof/ps), and so is 1Password
// (an in-memory vault) and the Secure Enclave (software keys).
type harness struct {
	t      *testing.T
	s      *Server
	srv    *httptest.Server
	home   string
	mu     sync.Mutex
	caller Caller
	chrome bool
	store  *memStore
	person *memPerson
	chip   *softChip
	peers  []*peer
}

func newHarness(t *testing.T) *harness {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "token"), []byte("test-secret"), 0o600)
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"extensionIds":["`+extID+`"],"onepassword":{"vault":"Agents"}}`), 0o600)
	os.WriteFile(filepath.Join(home, "identities.json"), []byte(`{"profiles":{},"sites":{"rules":{},"learned":{}}}`), 0o600)
	s, err := New(Options{Home: home, Port: 1})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, s: s, home: home, caller: Caller{PID: 4242, App: "claude", Cwd: "/work/my-repo"}, chrome: true,
		store: &memStore{items: testItems()}, person: &memPerson{}, chip: &softChip{}}
	s.caller = func(int) Caller { h.mu.Lock(); defer h.mu.Unlock(); return h.caller }
	s.isChrome = func(int) bool { h.mu.Lock(); defer h.mu.Unlock(); return h.chrome }
	s.vault = &vault.Vault{Settings: s.vaultSettings, Person: h.person, Connect: func(context.Context, string) (vault.Store, error) { return h.store, nil },
		Device: func() (string, string) { return "mac-a", "Sam's MacBook Pro" }}
	s.chip = h.chip
	h.srv = httptest.NewServer(s)
	t.Cleanup(func() {
		// Close doesn't wait for hijacked WebSockets: let their goroutines finish writing the log and audit trail
		// into home before the test removes it
		h.mu.Lock()
		for _, p := range h.peers {
			p.ws.Close()
		}
		h.mu.Unlock()
		h.srv.Close()
		for range 200 {
			s.mu.Lock()
			idle := len(s.agents) == 0 && len(s.profiles) == 0
			s.mu.Unlock()
			if idle {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
	})
	return h
}

func (h *harness) as(c Caller, chrome bool) {
	h.mu.Lock()
	h.caller, h.chrome = c, chrome
	h.mu.Unlock()
}

// peer: one WebSocket client; every message it gets is kept, next() waits for one not taken yet.
type peer struct {
	t      *testing.T
	ws     *websocket.Conn
	mu     sync.Mutex
	got    []map[string]any
	taken  map[int]bool
	code   int
	reason string
	done   chan struct{}
	answer func(m map[string]any) (any, string) // set for a fake extension
}

func (h *harness) dial(path string, header http.Header) *peer {
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.srv.URL, "http")+path, header)
	if err != nil {
		h.t.Fatal(err)
	}
	p := &peer{t: h.t, ws: ws, taken: map[int]bool{}, done: make(chan struct{})}
	h.mu.Lock()
	h.peers = append(h.peers, p)
	h.mu.Unlock()
	go func() {
		defer close(p.done)
		for {
			_, b, err := ws.ReadMessage()
			if err != nil {
				var ce *websocket.CloseError
				if errors.As(err, &ce) {
					p.mu.Lock()
					p.code, p.reason = ce.Code, ce.Text
					p.mu.Unlock()
				}
				return
			}
			var m map[string]any
			json.Unmarshal(b, &m)
			p.mu.Lock()
			p.got = append(p.got, m)
			answer := p.answer
			p.mu.Unlock()
			if answer != nil && m["op"] != nil {
				result, e := answer(m)
				reply := map[string]any{"id": m["id"], "result": result}
				if e != "" {
					reply = map[string]any{"id": m["id"], "error": e}
				}
				p.send(reply)
			}
		}
	}()
	h.t.Cleanup(func() { ws.Close() })
	return p
}

func (p *peer) send(v any) {
	b, _ := json.Marshal(v)
	p.mu.Lock()
	p.ws.WriteMessage(websocket.TextMessage, b)
	p.mu.Unlock()
}
func (p *peer) next(pred func(m map[string]any) bool) map[string]any {
	p.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		for i, m := range p.got {
			if !p.taken[i] && pred(m) {
				p.taken[i] = true
				p.mu.Unlock()
				return m
			}
		}
		p.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	b, _ := json.Marshal(p.got)
	p.t.Fatalf("no matching message; got %s", trunc(string(b), 1500))
	return nil
}
func (p *peer) closedWith() (int, string) {
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		p.t.Fatal("not closed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code, p.reason
}
func (p *peer) all(pred func(m map[string]any) bool) []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []map[string]any
	for _, m := range p.got {
		if pred(m) {
			out = append(out, m)
		}
	}
	return out
}

func id(n float64) func(m map[string]any) bool {
	return func(m map[string]any) bool { return m["id"] == n }
}
func method(name string) func(m map[string]any) bool {
	return func(m map[string]any) bool { return m["method"] == name || m["op"] == name }
}
func expr(m map[string]any) string {
	p, _ := m["params"].(map[string]any)
	e, _ := p["expression"].(string)
	return e
}
func errMsg(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		return fmt.Sprint(e["message"])
	}
	return ""
}

// fakeChrome: the extension in one Chrome profile; tabs from 100, records every op; page answers set by the test.
type fakeChrome struct {
	*peer
	mu      sync.Mutex
	tab     int
	focused any
	value   string
	fail    map[string]string // DevTools method -> error to answer with
	late    bool              // the page loaded while the debugger was off
	setup   map[string]any    // what otpsetup.js finds on the page: {uris, keys}
}

func (h *harness) extension(profile string) *fakeChrome {
	f := &fakeChrome{tab: 100, focused: map[string]any{"tag": "INPUT", "type": "password"}, fail: map[string]string{}}
	h.as(Caller{PID: 99}, true)
	f.peer = h.dial("/extension?profile="+profile+"&client=c-"+profile, http.Header{"Origin": {"chrome-extension://" + extID}})
	f.peer.mu.Lock()
	f.peer.answer = func(m map[string]any) (any, string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch m["op"] {
		case "create":
			f.tab++
			return map[string]any{"tabId": f.tab}, ""
		case "cdp":
			if e := f.fail[fmt.Sprint(m["method"])]; e != "" {
				return nil, e
			}
			if m["method"] == "Runtime.evaluate" {
				expr := fmt.Sprint(m["params"].(map[string]any)["expression"])
				if strings.Contains(expr, "BarcodeDetector") { // otpsetup.js
					return map[string]any{"result": map[string]any{"type": "object", "value": f.setup}}, ""
				}
				if strings.Contains(expr, "__agentBridgeLate") {
					return map[string]any{"result": map[string]any{"type": "boolean", "value": f.late}}, ""
				}
				if strings.Contains(expr, "activeElement") {
					return map[string]any{"result": map[string]any{"type": "object", "value": f.focused}}, ""
				}
				if !strings.Contains(expr, "__agentBridgeDone") {
					return map[string]any{"result": map[string]any{"type": "string", "value": f.value}}, ""
				}
			}
		}
		return map[string]any{}, ""
	}
	f.peer.mu.Unlock()
	f.next(method("config"))
	return f
}
func (f *fakeChrome) event(tab int, method string, params any) {
	f.send(map[string]any{"event": map[string]any{"tabId": tab, "method": method, "params": params}})
}

func (h *harness) agent(profile string) *peer {
	h.as(Caller{PID: 4242, App: "claude", Cwd: "/work/my-repo", ClaudeSession: "abc"}, false)
	q := "/devtools/browser/agent?token=test-secret"
	if profile != "" {
		q += "&profile=" + profile
	}
	return h.dial(q, nil)
}

// openTab: an agent tab, returning its session id and tab id.
func openTab(a *peer, n float64) (string, int) {
	a.send(map[string]any{"id": n, "method": "Target.createTarget", "params": map[string]any{"url": "about:blank"}})
	sid := a.next(id(n))["result"].(map[string]any)["targetId"].(string)
	return sid, tabOf(sid)
}

// ---- who may connect

func TestWhoMayConnect(t *testing.T) {
	h := newHarness(t)
	agent := func(q string, c Caller, header http.Header) (int, string) {
		h.as(c, false)
		p := h.dial("/devtools/browser/agent"+q, header)
		p.send(map[string]any{"id": 1, "method": "Browser.getVersion"})
		return p.closedWith()
	}
	claude := Caller{PID: 1, App: "claude"}
	if code, _ := agent("", claude, nil); code != 4001 {
		t.Error("no secret:", code)
	}
	if code, _ := agent("?token=nope", claude, nil); code != 4001 {
		t.Error("wrong secret:", code)
	}
	if code, reason := agent("?token=test-secret", Caller{PID: 1, Command: "node x"}, nil); code != 4003 || !strings.Contains(reason, "Claude Code, Codex, Cursor and Paseo") {
		t.Error("not an agent app:", code, reason)
	}
	if code, reason := agent("?token=test-secret", claude, http.Header{"Origin": {"https://evil.example"}}); code != 4003 || !strings.Contains(reason, "web pages") {
		t.Error("web page:", code, reason)
	}
	for _, app := range []string{"claude", "codex", "cursor", "paseo"} {
		h.as(Caller{PID: 1, App: app}, false)
		p := h.dial("/devtools/browser/agent?token=test-secret", nil)
		p.send(map[string]any{"id": 1, "method": "Browser.getVersion"})
		if m := p.next(id(1)); !strings.Contains(errMsg(m), "extension is not connected") {
			t.Error(app, "should be accepted and told there's no extension:", m)
		}
	}
	ext := func(origin string, chrome bool) (int, string) {
		h.as(Caller{PID: 2}, chrome)
		hd := http.Header{}
		if origin != "" {
			hd.Set("Origin", origin)
		}
		return h.dial("/extension?profile=x", hd).closedWith()
	}
	if code, reason := ext("", true); code != 4003 || !strings.Contains(reason, "not the Chrome Relay extension") {
		t.Error("no origin:", code, reason)
	}
	if code, reason := ext("chrome-extension://pponmlkjihgfedcbapponmlkjihgfedcb", true); code != 4003 || !strings.Contains(reason, "not the Chrome Relay extension") {
		t.Error("another extension:", code, reason)
	}
	if code, reason := ext("chrome-extension://"+extID, false); code != 4003 || !strings.Contains(reason, "not Chrome") {
		t.Error("not Chrome:", code, reason)
	}
	time.Sleep(100 * time.Millisecond)
	day := time.Now().UTC().Format("2006-01-02")
	b, _ := os.ReadFile(filepath.Join(h.home, "audit", day+".jsonl"))
	if !regexp.MustCompile(`"event":"session start".*"via"|"via":"claude".*"event":"session start"`).Match(b) {
		t.Error("sessions are audited:", string(b))
	}
}

// ---- tabs, setup replay, target metadata (main's fixes)

func TestTabsSetupReplayAndTargetInfo(t *testing.T) {
	h := newHarness(t)
	chrome := h.extension("me@example.com")
	a := h.agent("me@example.com")
	sid, tab := openTab(a, 1)
	if n := len(chrome.all(func(m map[string]any) bool { return m["op"] == "cdp" && m["tabId"] == float64(tab) })); n != 8 {
		t.Errorf("tab setup: %d DevTools commands, want 8 (focus, Page, Runtime, binding, popup script, passkey + quiet script, WebAuthn x2)", n)
	}
	auth := chrome.all(func(m map[string]any) bool { return m["method"] == "WebAuthn.addVirtualAuthenticator" })
	if len(auth) != 1 || !strings.Contains(fmt.Sprint(auth[0]["params"]), "automaticPresenceSimulation:false") {
		t.Error("Chrome's own passkey dialogs are kept out by an authenticator that never confirms:", auth)
	}
	cmd := func(n float64, method string) map[string]any {
		a.send(map[string]any{"id": n, "sessionId": sid, "method": method, "params": map[string]any{}})
		return a.next(id(n))
	}
	chrome.mu.Lock()
	chrome.fail["Network.enable"] = "Network setup failed"
	chrome.mu.Unlock()
	if !strings.Contains(errMsg(cmd(2, "Network.enable")), "Network setup failed") {
		t.Error("the failure reaches the agent")
	}
	chrome.mu.Lock()
	delete(chrome.fail, "Network.enable")
	chrome.mu.Unlock()
	cmd(3, "Log.enable")
	cmd(4, "Log.disable")
	cmd(5, "Page.setLifecycleEventsEnabled")
	before := len(chrome.all(func(map[string]any) bool { return true }))
	chrome.send(map[string]any{"paused": map[string]any{"tabId": tab, "reason": "target_closed"}})
	time.Sleep(50 * time.Millisecond)
	chrome.send(map[string]any{"reattached": map[string]any{"tabId": tab}})
	cmd(6, "Runtime.evaluate") // waits for the re-attach, then runs
	replayed := []string{}
	for _, m := range chrome.all(func(map[string]any) bool { return true })[before:] {
		if m["op"] == "cdp" && m["method"] != "Runtime.evaluate" {
			if !strings.HasPrefix(fmt.Sprint(m["method"]), "WebAuthn.") {
				replayed = append(replayed, fmt.Sprint(m["method"]))
			}
		}
	}
	if strings.Contains(strings.Join(replayed, ","), "Network.enable") || strings.Contains(strings.Join(replayed, ","), "Log.enable") ||
		!strings.Contains(strings.Join(replayed, ","), "Page.setLifecycleEventsEnabled") || replayed[0] != "Emulation.setFocusEmulationEnabled" {
		t.Error("replays only successful, still-active setup:", replayed)
	}
	if strings.Contains(strings.Join(replayed, ","), "Page.reload") {
		t.Error("the same page, its scripts in place: no reload", replayed)
	}
	// a page that loaded while the debugger was off runs without the page scripts: it is loaded again
	chrome.mu.Lock()
	chrome.late = true
	chrome.mu.Unlock()
	before = len(chrome.all(func(map[string]any) bool { return true }))
	chrome.send(map[string]any{"paused": map[string]any{"tabId": tab, "reason": "target_closed"}})
	time.Sleep(50 * time.Millisecond)
	chrome.send(map[string]any{"reattached": map[string]any{"tabId": tab}})
	cmd(61, "Runtime.evaluate")
	if len(chrome.all(func(m map[string]any) bool { return m["method"] == "Page.reload" })) != 1 {
		t.Error("a page loaded without the scripts is reloaded once")
	}
	chrome.mu.Lock()
	chrome.late = false
	chrome.mu.Unlock()

	a.send(map[string]any{"id": 7, "method": "Target.setDiscoverTargets", "params": map[string]any{"discover": true}})
	a.next(id(7))
	u := "https://fixture.example.test/work?view=active#section"
	chrome.event(tab, "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "main", "url": u}})
	if c := a.next(method("Target.targetInfoChanged")); c["params"].(map[string]any)["targetInfo"].(map[string]any)["url"] != u {
		t.Error("discovery gets the committed URL", c)
	}
	chrome.event(tab, "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "sub", "parentId": "main", "url": "https://x.test/iframe"}})
	chrome.event(tab, "Page.navigatedWithinDocument", map[string]any{"frameId": "child", "url": "https://x.test/sub"})
	a.send(map[string]any{"id": 8, "method": "Target.getTargetInfo", "params": map[string]any{"targetId": sid}})
	if got := a.next(id(8))["result"].(map[string]any)["targetInfo"].(map[string]any)["url"]; got != u {
		t.Error("subframes don't replace the target URL:", got)
	}
	if h.s.mu.Lock(); true {
		var ag *Agent
		for x := range h.s.agents {
			ag = x
		}
		if ag.urls[tab] != "https://fixture.example.test/work" {
			t.Error("audit URLs stay redacted:", ag.urls[tab])
		}
		h.s.mu.Unlock()
	}
	a.send(map[string]any{"id": 9, "method": "Target.closeTarget", "params": map[string]any{"targetId": sid}})
	a.next(id(9))
	a.send(map[string]any{"id": 10, "method": "Target.getTargetInfo", "params": map[string]any{"targetId": sid}})
	if !strings.Contains(errMsg(a.next(id(10))), "No target") {
		t.Error("a closed target is an error, not fabricated metadata")
	}
}

func TestDisconnectDuringCreationClosesTheLateTab(t *testing.T) {
	h := newHarness(t)
	chrome := h.extension("me@example.com")
	release := make(chan struct{})
	chrome.peer.mu.Lock()
	inner := chrome.peer.answer
	chrome.peer.answer = func(m map[string]any) (any, string) {
		if m["op"] == "create" {
			<-release
		}
		return inner(m)
	}
	chrome.peer.mu.Unlock()
	a := h.agent("me@example.com")
	a.send(map[string]any{"id": 1, "method": "Target.createTarget", "params": map[string]any{}})
	chrome.next(method("create"))
	a.ws.Close()
	time.Sleep(100 * time.Millisecond)
	close(release)
	if m := chrome.next(method("close")); m["tabId"] != float64(101) {
		t.Error("the tab Chrome made after the agent left is closed:", m)
	}
}

func TestSocketsClosedDuringChecksAreDiscarded(t *testing.T) {
	h := newHarness(t)
	slow := make(chan struct{})
	h.s.caller = func(int) Caller { <-slow; return Caller{PID: 1, App: "claude"} }
	p := h.dial("/devtools/browser/agent?token=test-secret", nil)
	p.ws.Close()
	time.Sleep(50 * time.Millisecond)
	close(slow)
	time.Sleep(100 * time.Millisecond)
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if len(h.s.agents) != 0 {
		t.Error("an agent that left during its check is not kept")
	}
}

// ---- 1Password: fills, masking, refusals, approvals

func TestVaultFillsSecretsAgentsNeverSee(t *testing.T) {
	h := newHarness(t)
	chrome := h.extension("me@example.com")
	a := h.agent("me@example.com")
	sid, tab := openTab(a, 1)
	n := 10.0
	cmd := func(method string, params any) map[string]any {
		n++
		a.send(map[string]any{"id": n, "sessionId": sid, "method": method, "params": params})
		return a.next(id(n))
	}
	nav := func(u string) {
		chrome.event(tab, "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "F", "url": u}})
		a.next(func(m map[string]any) bool {
			return m["method"] == "Page.frameNavigated" && m["params"].(map[string]any)["frame"].(map[string]any)["url"] == u
		})
	}
	typed := func() []string {
		var out []string
		for _, m := range chrome.all(func(m map[string]any) bool { return m["method"] == "Input.insertText" }) {
			out = append(out, fmt.Sprint(m["params"].(map[string]any)["text"]))
		}
		return out
	}
	nav("https://github.com/login")
	if r := cmd("Input.insertText", map[string]any{"text": "op://Agents/GitHub/password"}); r["result"] == nil {
		t.Fatal("fill:", r)
	}
	if fmt.Sprint(typed()) != "[hunter2-secret]" {
		t.Fatal("typed", typed())
	}
	if !strings.Contains(fmt.Sprint(h.person.prompts), `let Claude Code (my-repo) fill the password of "GitHub" on github.com`) {
		t.Error("Touch ID names the agent, item and site:", h.person.prompts)
	}
	chrome.mu.Lock()
	chrome.value = "hunter2-secret"
	chrome.mu.Unlock()
	if r := cmd("Runtime.evaluate", map[string]any{"expression": "document.querySelector('input').value"}); r["result"].(map[string]any)["result"].(map[string]any)["value"] != "••••••••" {
		t.Error("read back masked:", r)
	}
	chrome.mu.Lock()
	chrome.focused = map[string]any{"tag": "INPUT", "type": "text"}
	chrome.mu.Unlock()
	if !strings.Contains(errMsg(cmd("Input.insertText", map[string]any{"text": "op://Agents/GitHub/password"})), "only goes into a password field") {
		t.Error("secrets only into password fields")
	}
	cmd("Input.insertText", map[string]any{"text": "op://Agents/GitHub/username"})
	cmd("Input.insertText", map[string]any{"text": "op://Agents/GitHub/otp"})
	got := typed()
	if code, _, _ := vault.TOTP("JBSWY3DPEHPK3PXP", time.Now()); len(got) != 3 || got[1] != "agent-bot" || (got[2] != code && len(got[2]) != 6) {
		t.Error("username and the current one-time code:", got)
	}
	if len(h.person.approvals) != 1 {
		t.Error("one sign-in, one approval:", h.person.approvals)
	}
	// a code split over six one-character boxes: one character per box, each focused in turn
	chrome.mu.Lock()
	chrome.focused = map[string]any{"tag": "INPUT", "type": "tel", "boxes": 6}
	chrome.mu.Unlock()
	cmd("Input.insertText", map[string]any{"text": "op://Agents/GitHub/otp"})
	if got := typed()[3:]; len(got) != 6 || strings.Join(got, "") != typed()[2] {
		t.Error("the code goes one digit per box:", got)
	}
	cmd("Input.insertText", map[string]any{"text": "AM54RQ"}) // a code the agent read from an email
	if got := typed()[9:]; strings.Join(got, "") != "AM54RQ" || len(got) != 6 {
		t.Error("a typed code goes one character per box too:", got)
	}
	chrome.mu.Lock()
	chrome.focused = map[string]any{"tag": "INPUT", "type": "text"}
	chrome.mu.Unlock()
	cmd("Input.insertText", map[string]any{"text": "op://Agents/GitHub/username"})
	// a password field in a frame of the page's own origin (Apple's sign-in form) is the page; another site's frame is not
	chrome.mu.Lock()
	chrome.focused = map[string]any{"tag": "INPUT", "type": "password", "inner": true}
	chrome.mu.Unlock()
	if r := cmd("Input.insertText", map[string]any{"text": "op://Agents/GitHub/password"}); r["result"] == nil || typed()[len(typed())-1] != "hunter2-secret" {
		t.Error("a same-origin frame's password field is filled:", r)
	}
	chrome.mu.Lock()
	chrome.focused = map[string]any{"frame": true, "focus": true}
	chrome.mu.Unlock()
	if !strings.Contains(errMsg(cmd("Input.insertText", map[string]any{"text": "op://Agents/GitHub/password"})), "another site's frame") {
		t.Error("another site's frame is refused")
	}
	chrome.mu.Lock()
	chrome.focused = map[string]any{"tag": "INPUT", "type": "password"}
	chrome.mu.Unlock()
	nav("https://evil.example/login")
	if !strings.Contains(errMsg(cmd("Input.insertText", map[string]any{"text": "op://Agents/GitHub/password"})), "is for https://github.com, not evil.example") {
		t.Error("other sites are refused")
	}
	if !strings.Contains(errMsg(cmd("Input.insertText", map[string]any{"text": "op://Private/GitHub/password"})), `only the vaults "Agents"`) {
		t.Error("other vaults are refused")
	}
	if len(typed()) != 17 {
		t.Error("nothing typed on refusal")
	}
	a.mu.Lock()
	all, _ := json.Marshal(a.got)
	a.mu.Unlock()
	if strings.Contains(string(all), "hunter2-secret") {
		t.Error("the agent received the password")
	}
	time.Sleep(100 * time.Millisecond)
	b, _ := os.ReadFile(filepath.Join(h.home, "audit", time.Now().UTC().Format("2006-01-02")+".jsonl"))
	if !strings.Contains(string(b), `"event":"1password fill","tab":`+fmt.Sprint(tab)+`,"item":"GitHub","field":"passwd"`) || !strings.Contains(string(b), `"frame":"same origin"`) || !strings.Contains(string(b), "not evil.example") || strings.Contains(string(b), "hunter2") {
		t.Error("audit: fills and refusals, no secrets:", string(b))
	}
	res, err := http.Get(h.srv.URL + "/vault?token=test-secret")
	if err != nil {
		t.Fatal(err)
	}
	var sum vault.Summary
	json.NewDecoder(res.Body).Decode(&sum)
	if len(sum.Items) != 2 || sum.Items[0].Title != "GitHub" || !sum.Items[0].OTP {
		t.Error("vault listing", sum)
	}
	if res, _ := http.Get(h.srv.URL + "/vault"); res.StatusCode != 404 {
		t.Error("listing needs the secret")
	}
}

// ---- passkeys: Secure Enclave (software here), page-only, handoff to 1Password

func TestPasskeysForThePageItselfAndHandoffs(t *testing.T) {
	h := newHarness(t)
	h.s.handoffWait = 300 * time.Millisecond
	var alerts [][]string
	h.s.notify = func(args ...string) { alerts = append(alerts, args) }
	chrome := h.extension("me@example.com")
	a := h.agent("me@example.com")
	sid, tab := openTab(a, 1)
	nav := func(u string, ctx int) {
		chrome.event(tab, "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "TOP", "url": u}})
		chrome.event(tab, "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": ctx, "auxData": map[string]any{"frameId": "TOP", "isDefault": true}}})
		a.next(func(m map[string]any) bool {
			p, _ := m["params"].(map[string]any)
			c, _ := p["context"].(map[string]any)
			return m["method"] == "Runtime.executionContextCreated" && c["id"] == float64(ctx)
		})
	}
	ask := func(call map[string]any, ctx int) {
		call["op"], call["mediation"] = "webauthn", ""
		raw, _ := json.Marshal(call)
		chrome.event(tab, "Runtime.bindingCalled", map[string]any{"name": binding, "executionContextId": ctx, "payload": string(raw)})
	}
	answer := func(n int) map[string]any {
		m := chrome.next(func(m map[string]any) bool {
			e := expr(m)
			return strings.Contains(e, fmt.Sprintf("__agentBridgeDone(%d,", n))
		})
		e := expr(m)
		var r map[string]any
		json.Unmarshal([]byte(e[strings.Index(e, ", ")+2:len(e)-1]), &r)
		return r
	}
	b := func(x []byte) map[string]string {
		return map[string]string{"$b": base64.RawURLEncoding.EncodeToString(x)}
	}
	challenge := []byte("challenge-one-two-three")
	creation := map[string]any{"rp": map[string]any{"id": "webauthn.example", "name": "Demo"}, "user": map[string]any{"id": b([]byte{7, 7}), "name": "bot"},
		"challenge": b(challenge), "pubKeyCredParams": []any{map[string]any{"type": "public-key", "alg": -7}}}

	nav("https://webauthn.example/register", 1)
	ask(map[string]any{"id": 1, "kind": "create", "publicKey": creation}, 1)
	if r := answer(1); !strings.Contains(fmt.Sprint(r["message"]), "only right after the agent clicked or typed") {
		t.Error("creating needs the agent's own click first:", r)
	}
	a.send(map[string]any{"id": 2, "sessionId": sid, "method": "Input.dispatchMouseEvent", "params": map[string]any{"type": "mousePressed", "x": 1, "y": 1}})
	a.next(id(2))
	ask(map[string]any{"id": 2, "kind": "create", "publicKey": creation}, 1)
	made := answer(2)["credential"].(map[string]any)
	resp := made["response"].(map[string]any)
	d := func(s any) []byte { x, _ := base64.RawURLEncoding.DecodeString(s.(string)); return x }
	var cd map[string]any
	json.Unmarshal(d(resp["clientDataJSON"]), &cd)
	if cd["origin"] != "https://webauthn.example" || cd["type"] != "webauthn.create" {
		t.Error("clientData", cd)
	}
	saved := h.store.items[len(h.store.items)-1]
	if saved.Title != "Demo passkey (bot) · Chrome Relay · Sam's MacBook Pro" || !slices.Contains(saved.Tags, vault.PasskeyTag) || vault.PasskeyOf(saved) == nil || strings.Contains(fmt.Sprint(saved), "private") {
		t.Error("saved to the vault, no private key:", saved.Title)
	}

	nav("https://webauthn.example/login", 2)
	ask(map[string]any{"id": 3, "kind": "get", "publicKey": map[string]any{"challenge": b([]byte("second")), "rpId": "webauthn.example",
		"allowCredentials": []any{map[string]any{"type": "public-key", "id": map[string]string{"$b": made["id"].(string)}}}}}, 2)
	got := answer(3)["credential"].(map[string]any)["response"].(map[string]any)
	pub, _ := x509.ParsePKIXPublicKey(d(resp["publicKey"]))
	ch := sha256.Sum256(d(got["clientDataJSON"]))
	signed := sha256.Sum256(append(d(got["authenticatorData"]), ch[:]...))
	if !ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), signed[:], d(got["signature"])) {
		t.Error("the site cannot verify the sign-in")
	}
	if want := "let Claude Code (my-repo) sign in to webauthn.example as bot, from webauthn.example"; len(h.chip.reasons) == 0 || h.chip.reasons[0] != want {
		t.Errorf("Touch ID reason %q, want %q", h.chip.reasons, want)
	}

	chrome.event(tab, "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 3, "auxData": map[string]any{"frameId": "AD-FRAME", "isDefault": true}}})
	ask(map[string]any{"id": 4, "kind": "get", "publicKey": map[string]any{"challenge": b([]byte("x")), "rpId": "webauthn.example"}}, 3)
	nav("https://other.example/login", 4)
	ask(map[string]any{"id": 5, "kind": "get", "publicKey": map[string]any{"challenge": b([]byte("x"))}}, 4)
	if r := answer(5); r["handoff"] != true {
		t.Error("not the relay's: handed to 1Password", r)
	}
	chrome.next(func(m map[string]any) bool { return m["op"] == "handoff" && m["rpId"] == "other.example" })
	if len(alerts) != 1 || alerts[0][0] != "notify" || !strings.Contains(alerts[0][3], "other.example") || alerts[0][5] == "" || alerts[0][6] != fmt.Sprint(tab) {
		t.Error("the person is told through Chrome Relay.app, with the profile and tab a click opens:", alerts)
	}
	ask(map[string]any{"id": 6, "kind": "get", "publicKey": map[string]any{"challenge": b([]byte("x")), "rpId": "webauthn.example"}}, 4)
	if r := answer(6); r["error"] != "SecurityError" {
		t.Error("another domain's passkey:", r)
	}
	if len(chrome.all(func(m map[string]any) bool {
		e := expr(m)
		return strings.Contains(e, "__agentBridgeDone(4,")
	})) != 0 {
		t.Error("a frame inside the page gets no answer")
	}
	// 1Password's prompt is in the page: commands wait, then say why
	chrome.send(map[string]any{"paused": map[string]any{"tabId": tab, "reason": "target_closed"}})
	time.Sleep(50 * time.Millisecond)
	a.send(map[string]any{"id": 7, "sessionId": sid, "method": "Runtime.evaluate", "params": map[string]any{"expression": "1"}})
	if !strings.Contains(errMsg(a.next(id(7))), "PASSKEY APPROVAL NEEDED") {
		t.Error("the agent is told to ask the person")
	}
	a.send(map[string]any{"id": 8, "sessionId": sid, "method": "Runtime.evaluate", "params": map[string]any{"expression": "1"}})
	chrome.send(map[string]any{"reattached": map[string]any{"tabId": tab}})
	if a.next(id(8))["result"] == nil {
		t.Error("the tab works again after approval")
	}
}

// ---- doubles

type memStore struct {
	mu    sync.Mutex
	items []op.Item
}

func testItems() []op.Item {
	return []op.Item{
		{ID: "gh1", Title: "GitHub", Category: op.ItemCategoryLogin, Websites: []op.Website{{URL: "https://github.com"}}, Fields: []op.ItemField{
			{ID: "username", Title: "username", FieldType: op.ItemFieldTypeText, Value: "agent-bot"},
			{ID: "password", Title: "passwd", FieldType: op.ItemFieldTypeConcealed, Value: "hunter2-secret"},
			{ID: "otp", Title: "one-time password", FieldType: op.ItemFieldTypeTOTP, Value: "otpauth://totp/x?secret=JBSWY3DPEHPK3PXP"}}},
		{ID: "nw1", Title: "No website", Category: op.ItemCategoryLogin, Fields: []op.ItemField{{ID: "password", FieldType: op.ItemFieldTypeConcealed, Value: "x"}}},
	}
}
func (s *memStore) Vaults(context.Context) ([]op.VaultOverview, error) {
	return []op.VaultOverview{{ID: "v", Title: "Agents"}}, nil
}
func (s *memStore) List(context.Context, string) ([]op.ItemOverview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []op.ItemOverview
	for _, it := range s.items {
		out = append(out, op.ItemOverview{ID: it.ID, Title: it.Title, Category: it.Category, Websites: it.Websites})
	}
	return out, nil
}
func (s *memStore) Get(_ context.Context, _, id string) (op.Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items {
		if it.ID == id {
			return it, nil
		}
	}
	return op.Item{}, errors.New("no item")
}
func (s *memStore) Create(_ context.Context, p op.ItemCreateParams) (op.Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := op.Item{ID: fmt.Sprint("new", len(s.items)), Title: p.Title, Category: p.Category, Fields: p.Fields, Sections: p.Sections, Websites: p.Websites, Tags: p.Tags}
	s.items = append(s.items, it)
	return it, nil
}
func (s *memStore) Put(_ context.Context, it op.Item) (op.Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == it.ID {
			s.items[i] = it
		}
	}
	return it, nil
}

type memPerson struct {
	mu        sync.Mutex
	prompts   []string // opening the token, and approvals
	approvals []string // approvals only (Touch ID here or the iPhone)
	deny      bool
}

func (p *memPerson) Unlock(key, reason string) (vault.Token, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prompts = append(p.prompts, reason)
	return vault.Token{Value: "ops_x", Approve: "sign-in"}, nil
}
func (p *memPerson) Approve(d phone.Details) error {
	reason := d.Reason
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prompts = append(p.prompts, reason)
	p.approvals = append(p.approvals, reason)
	if p.deny {
		return errors.New("the person denied it on their iPhone")
	}
	return nil
}

type softChip struct {
	legacy  bool // make keys like Chrome Relay did before: the chip asks for Touch ID itself
	mu      sync.Mutex
	keys    []*ecdsa.PrivateKey
	reasons []string
	scopes  []string
	reuses  []time.Duration
}

func (c *softChip) Create() ([]byte, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	c.keys = append(c.keys, k)
	if c.legacy {
		return []byte{byte(len(c.keys) - 1)}, pub(k), nil
	}
	return []byte(keychainHandle + strconv.Itoa(len(c.keys)-1)), pub(k), nil
}

// pub: the uncompressed point, as the Secure Enclave gives it
func pub(k *ecdsa.PrivateKey) []byte { b, _ := k.PublicKey.Bytes(); return b }

func (c *softChip) Sign(handle, data []byte, reason, scope string, reuse time.Duration) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reasons = append(c.reasons, reason)
	c.scopes, c.reuses = append(c.scopes, scope), append(c.reuses, reuse)
	h := sha256.Sum256(data)
	if ref, ok := bytes.CutPrefix(handle, []byte(keychainHandle)); ok {
		i, _ := strconv.Atoi(string(ref))
		return ecdsa.SignASN1(rand.Reader, c.keys[i], h[:])
	}
	return ecdsa.SignASN1(rand.Reader, c.keys[handle[0]], h[:])
}

func TestExtensionClosingDuringItsCheckIsNotRegistered(t *testing.T) {
	h := newHarness(t)
	slow := make(chan struct{})
	h.s.caller = func(int) Caller { <-slow; return Caller{PID: 9} }
	p := h.dial("/extension?profile=x&client=c", http.Header{"Origin": {"chrome-extension://" + extID}})
	p.ws.Close()
	time.Sleep(50 * time.Millisecond)
	close(slow)
	time.Sleep(100 * time.Millisecond)
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if len(h.s.profiles) != 0 || h.s.watchers.Load() != 0 {
		t.Error("an extension that left during its check is not kept")
	}
}

func TestDisconnectDuringPopupCreationClosesTheLateTab(t *testing.T) {
	h := newHarness(t)
	chrome := h.extension("me@example.com")
	a := h.agent("me@example.com")
	_, tab := openTab(a, 1)
	chrome.next(method("create")) // the tab's own creation
	release := make(chan struct{})
	chrome.peer.mu.Lock()
	inner := chrome.peer.answer
	chrome.peer.answer = func(m map[string]any) (any, string) {
		if m["op"] == "create" {
			<-release
		}
		return inner(m)
	}
	chrome.peer.mu.Unlock()
	chrome.event(tab, "Runtime.bindingCalled", map[string]any{"name": binding, "executionContextId": 1, "payload": `{"op":"open","id":1,"url":"https://example.com/"}`})
	chrome.next(method("create"))
	a.ws.Close()
	time.Sleep(100 * time.Millisecond)
	close(release)
	if m := chrome.next(func(m map[string]any) bool { return m["op"] == "close" && m["tabId"] == float64(102) }); m == nil {
		t.Error("the popup Chrome made after the agent left is closed")
	}
}

func TestIdentityWatchersFollowExtensionConnections(t *testing.T) {
	h := newHarness(t)
	h.s.watchEvery = 20 * time.Millisecond
	ids := filepath.Join(h.home, "identities.json")
	touch := func(rule string) {
		os.WriteFile(ids, []byte(`{"profiles":{},"sites":{"rules":{"x.test":"`+rule+`"},"learned":{}}}`), 0o600)
		later := time.Now().Add(time.Second)
		os.Chtimes(ids, later, later)
	}
	configWith := func(p *fakeChrome, rule string) {
		t.Helper()
		p.next(func(m map[string]any) bool {
			sa, _ := m["siteAccounts"].(map[string]any)
			return m["op"] == "config" && sa["x.test"] == rule
		})
	}
	for range 3 { // reconnects release their watcher
		p := h.extension("a@example.com")
		if n := h.s.watchers.Load(); n != 1 {
			t.Fatal("watchers while connected:", n)
		}
		p.ws.Close()
		deadline := time.Now().Add(2 * time.Second)
		for h.s.watchers.Load() != 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if n := h.s.watchers.Load(); n != 0 {
			t.Fatal("a closed profile keeps its watcher:", n)
		}
	}
	a, b := h.extension("a@example.com"), h.extension("b@example.com")
	touch("one@example.com")
	configWith(a, "one@example.com")
	configWith(b, "one@example.com")
	a.ws.Close()
	time.Sleep(100 * time.Millisecond)
	touch("two@example.com")
	configWith(b, "two@example.com") // the other profile keeps getting updates
	// a replacement connection from the same install keeps its own watcher when the old socket closes
	c := h.extension("b@example.com")
	b.ws.Close()
	time.Sleep(100 * time.Millisecond)
	touch("three@example.com")
	configWith(c, "three@example.com")
}

func TestPasskeysCanBeTurnedOff(t *testing.T) {
	h := newHarness(t)
	os.WriteFile(filepath.Join(h.home, "config.json"), []byte(`{"extensionIds":["`+extID+`"],"passkeys":{"enabled":false}}`), 0o600)
	chrome := h.extension("me@example.com")
	a := h.agent("me@example.com")
	_, tab := openTab(a, 1)
	scripts := chrome.all(func(m map[string]any) bool {
		return m["tabId"] == float64(tab) && m["method"] == "Page.addScriptToEvaluateOnNewDocument"
	})
	for _, m := range scripts {
		if strings.Contains(fmt.Sprint(m["params"]), "__agentBridgeDone") {
			t.Fatal("passkeys off: no passkey script in agent tabs")
		}
	}
	if len(scripts) != 2 || !strings.Contains(fmt.Sprint(scripts[1]["params"]), "data-1p-ignore") {
		t.Fatal("the popup and password-manager scripts stay:", len(scripts))
	}

}

// Passkeys Chrome Relay makes are approved by Chrome Relay (Touch ID here or the paired iPhone, via the Person);
// one approval may cover an agent's sign-ins for the reuse window. Passkeys made before keep the chip's own
// Touch ID.
func TestPasskeyApprovalsMacOrIPhone(t *testing.T) {
	h := newHarness(t)
	setConfig := func(reuse int) {
		os.WriteFile(filepath.Join(h.home, "config.json"), []byte(fmt.Sprintf(`{"extensionIds":["`+extID+`"],"onepassword":{"vault":"Agents"},"passkeys":{"touchIdReuseSeconds":%d}}`, reuse)), 0o600)
	}
	setConfig(120)
	passkey := func(id, site, user string, legacy bool) {
		h.chip.legacy = legacy
		handle, pub, _ := h.chip.Create()
		sec := "s"
		f := func(t, v string) op.ItemField { return op.ItemField{Title: t, SectionID: &sec, Value: v} }
		h.store.items = append(h.store.items, op.Item{ID: id, Title: id, Category: op.ItemCategoryLogin, Websites: []op.Website{{URL: "https://" + site}},
			Sections: []op.ItemSection{{ID: "s", Title: vault.PasskeySection}}, Fields: []op.ItemField{f("rp id", site),
				f("credential id", base64.RawURLEncoding.EncodeToString([]byte(id))), f("user name", user),
				f("public key", base64.RawURLEncoding.EncodeToString(pub)), f("secure enclave key", base64.RawURLEncoding.EncodeToString(handle))}})
	}
	passkey("new-a", "a.example", "bot-a", false)
	passkey("new-b", "b.example", "bot-b", false)
	passkey("old-c", "c.example", "bot-c", true)
	chrome := h.extension("me@example.com")
	a := h.agent("me@example.com")
	n := 0
	signIn := func(site string) string {
		n++
		_, tab := openTab(a, float64(n))
		chrome.event(tab, "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "TOP", "url": "https://" + site + "/login"}})
		chrome.event(tab, "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 5, "auxData": map[string]any{"frameId": "TOP", "isDefault": true}}})
		a.next(func(m map[string]any) bool { return m["method"] == "Runtime.executionContextCreated" })
		chrome.event(tab, "Runtime.bindingCalled", map[string]any{"name": binding, "executionContextId": 5,
			"payload": `{"op":"webauthn","id":1,"kind":"get","mediation":"","publicKey":{"challenge":{"$b":"AAAA"},"rpId":"` + site + `"}}`})
		return expr(chrome.next(func(m map[string]any) bool {
			return m["tabId"] == float64(tab) && strings.Contains(expr(m), "__agentBridgeDone(1,")
		}))
	}
	approvals := func() []string {
		h.person.mu.Lock()
		defer h.person.mu.Unlock()
		return slices.Clone(h.person.approvals)
	}

	if r := signIn("a.example"); !strings.Contains(r, `"credential"`) {
		t.Fatal("signed in:", r)
	}
	got := approvals()
	if len(got) != 1 || !strings.Contains(got[0], "sign in to a.example as bot-a") || !strings.HasSuffix(got[0], "(and use its other passkeys for the next 2 minutes without asking)") {
		t.Fatal("one approval, naming the site and account and the reuse window:", got)
	}
	if r := signIn("b.example"); !strings.Contains(r, `"credential"`) || len(approvals()) != 1 {
		t.Fatal("within the window, the same agent's next passkey needs no approval:", r, approvals())
	}

	if r := signIn("c.example"); !strings.Contains(r, `"credential"`) || len(approvals()) != 1 {
		t.Fatal("a passkey made before: the chip's own Touch ID, not Chrome Relay's approval:", r, approvals())
	}

	setConfig(0)
	a = h.agent("me@example.com") // a new agent: no window of its own
	h.person.mu.Lock()
	h.person.deny = true
	h.person.mu.Unlock()
	if r := signIn("a.example"); !strings.Contains(r, "NotAllowedError") || !strings.Contains(r, "denied it on their iPhone") {
		t.Fatal("denied on the iPhone: the page is refused:", r)
	}
	h.person.mu.Lock()
	h.person.deny = false
	h.person.mu.Unlock()
	signIn("a.example")
	signIn("b.example")
	if got := approvals(); len(got) != 4 || strings.Contains(got[3], "without asking") {
		t.Fatal("no reuse window: every sign-in asks:", got)
	}
}

// A site rule only reaches profiles where its Google account is signed in: a work rule doesn't send a personal
// profile's Google hop looking for the work account.
func TestSiteRulesFollowTheProfilesAccounts(t *testing.T) {
	for prof, want := range map[string]string{"work@acme.com": "work@acme.com", "me@gmail.com": ""} {
		h := newHarness(t)
		os.WriteFile(filepath.Join(h.home, "identities.json"), []byte(`{"profiles":{
			"work@acme.com":{"googleAccounts":["work@acme.com","me@gmail.com"]},
			"me@gmail.com":{"googleAccounts":["me@gmail.com"]}},
			"sites":{"rules":{"myaccount.google.com":"work@acme.com","mail.example":"me@gmail.com"},"learned":{}}}`), 0o600)
		p := h.extension(prof)
		cfgs := p.all(func(m map[string]any) bool { return m["op"] == "config" }) // the harness has read it already
		if len(cfgs) == 0 {
			t.Fatal(prof, "no config")
		}
		sa, _ := cfgs[0]["siteAccounts"].(map[string]any)
		got, _ := sa["myaccount.google.com"].(string)
		if got != want || sa["mail.example"] != "me@gmail.com" {
			t.Errorf("%s: %v", prof, sa)
		}
	}
}

func TestAgentsSaveNewLoginsAndOneTimeCodesTheyNeverSee(t *testing.T) {
	h := newHarness(t)
	chrome := h.extension("me@example.com")
	a := h.agent("me@example.com")
	sid, tab := openTab(a, 1)
	n := 10.0
	cmd := func(method string, params any) map[string]any {
		n++
		a.send(map[string]any{"id": n, "sessionId": sid, "method": method, "params": params})
		return a.next(id(n))
	}
	typed := func() []string {
		var out []string
		for _, m := range chrome.all(func(m map[string]any) bool { return m["method"] == "Input.insertText" }) {
			out = append(out, fmt.Sprint(m["params"].(map[string]any)["text"]))
		}
		return out
	}
	chrome.event(tab, "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "F", "url": "https://app.acme.test/signup"}})
	a.next(func(m map[string]any) bool { return m["method"] == "Page.frameNavigated" })

	ref := "op://Agents/Acme/password?new&username=bot@acme.test"
	if r := cmd("Input.insertText", map[string]any{"text": ref}); r["result"] == nil {
		t.Fatal("new password:", r)
	}
	if r := cmd("Input.insertText", map[string]any{"text": ref}); r["result"] == nil { // "confirm password"
		t.Fatal("confirm:", r)
	}
	got := typed()
	if len(got) != 2 || got[0] != got[1] || len(got[0]) != 24 {
		t.Fatal("the same new password, twice:", got)
	}
	saved := h.store.items[len(h.store.items)-1]
	if saved.Title != "Acme" || vault.PickField(saved, "password", false).Value != got[0] || !slices.Contains(saved.Tags, vault.SavedTag) {
		t.Fatalf("saved in 1Password: %+v", saved)
	}
	if len(h.person.approvals) != 1 || !strings.Contains(h.person.approvals[0], "save a new login for app.acme.test as bot@acme.test") {
		t.Fatal("one approval for the save:", h.person.approvals)
	}
	chrome.mu.Lock()
	chrome.value = got[0]
	chrome.focused = map[string]any{"tag": "INPUT", "type": "text"}
	chrome.mu.Unlock()
	if r := cmd("Runtime.evaluate", map[string]any{"expression": "document.querySelector('input').value"}); r["result"].(map[string]any)["result"].(map[string]any)["value"] != "••••••••" {
		t.Error("read back masked:", r)
	}
	if !strings.Contains(errMsg(cmd("Input.insertText", map[string]any{"text": "op://Agents/Other/password?new&username=x@acme.test"})), "only goes into a password field") {
		t.Error("a new password only into a password field")
	}

	// 2-step: the setup key comes from the page, never from the agent
	chrome.mu.Lock()
	chrome.setup = map[string]any{"uris": []any{}, "keys": []any{}}
	chrome.mu.Unlock()
	if !strings.Contains(errMsg(cmd("Input.insertText", map[string]any{"text": "op://Agents/Acme/otp?new"})), "shows no 2-step setup key") {
		t.Error("no key on the page")
	}
	chrome.mu.Lock()
	chrome.setup = map[string]any{"uris": []any{"otpauth://totp/Acme:bot?secret=JBSWY3DPEHPK3PXP&issuer=Acme"}, "keys": []any{"JBSWY3DPEHPK3PXP"}}
	chrome.mu.Unlock()
	if r := cmd("Input.insertText", map[string]any{"text": "op://Agents/Acme/otp?new"}); r["result"] == nil {
		t.Fatal("2-step:", r)
	}
	if code, _, _ := vault.TOTP("JBSWY3DPEHPK3PXP", time.Now()); typed()[2] != code {
		t.Error("the current code is typed:", typed())
	}
	if f := vault.PickField(h.store.items[len(h.store.items)-1], "otp", true); f == nil || !strings.Contains(f.Value, "JBSWY3DPEHPK3PXP") {
		t.Error("the setup key is saved with the login")
	}
	a.mu.Lock()
	all, _ := json.Marshal(a.got)
	a.mu.Unlock()
	if strings.Contains(string(all), got[0]) || strings.Contains(string(all), "JBSWY3DPEHPK3PXP") {
		t.Error("the agent received a secret")
	}
	time.Sleep(100 * time.Millisecond)
	b, _ := os.ReadFile(filepath.Join(h.home, "audit", time.Now().UTC().Format("2006-01-02")+".jsonl"))
	if !strings.Contains(string(b), `"event":"1password saved login"`) || !strings.Contains(string(b), `"event":"1password saved one-time code"`) || strings.Contains(string(b), got[0]) {
		t.Error("audit: saves, no secrets:", string(b))
	}
}

// Two copies of the extension in one profile (the Web Store's, allowed without setup recording it, and an unpacked
// one): the second is told so, and /versions says which copy each profile runs.
func TestTwoCopiesOfTheExtension(t *testing.T) {
	h := newHarness(t)
	h.extension("me@example.com")
	h.as(Caller{PID: 99}, true)
	second := h.dial("/extension?profile=me@example.com&client=another", http.Header{"Origin": {"chrome-extension://" + WebStoreID}})
	if code, reason := second.closedWith(); code != 4009 || !strings.Contains(reason, "another copy of Chrome Relay") {
		t.Error("a second copy in the same profile:", code, reason)
	}
	res, err := http.Get(h.srv.URL + "/versions")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var v map[string]struct {
		Version, ID string
		WebStore    bool
	}
	json.NewDecoder(res.Body).Decode(&v)
	if got := v["me@example.com"]; got.ID != extID || got.WebStore {
		t.Error("versions:", v)
	}
	h.as(Caller{PID: 99}, true)
	h.dial("/extension?profile=other@example.com&client=store", http.Header{"Origin": {"chrome-extension://" + WebStoreID}})
	time.Sleep(200 * time.Millisecond)
	h.s.mu.Lock()
	connected := h.s.profiles["other@example.com"] != nil
	h.s.mu.Unlock()
	if !connected {
		t.Error("the Web Store's copy connects without setup recording its ID")
	}
}
