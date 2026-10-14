package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// driverChrome: the fake extension, also answering the driver's page scripts.
func driverChrome(h *harness, drive func(expr string) any) *fakeChrome {
	chrome := h.extension("me@example.com")
	chrome.peer.mu.Lock()
	orig := chrome.peer.answer
	chrome.peer.answer = func(m map[string]any) (any, string) {
		if m["op"] == "cdp" && m["method"] == "Page.getFrameTree" {
			return map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "main"}}}, ""
		}
		if m["op"] == "cdp" && m["method"] == "Page.createIsolatedWorld" { // the driver's scripts run in a world of their own
			return map[string]any{"executionContextId": 7}, ""
		}
		if m["op"] == "cdp" && m["method"] == "Runtime.evaluate" {
			if e := expr(m); strings.Contains(e, "__crDrive") {
				return map[string]any{"result": map[string]any{"type": "object", "value": drive(e)}}, ""
			}
		}
		return orig(m)
	}
	chrome.peer.mu.Unlock()
	return chrome
}

func (h *harness) drive(path string, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	// a new connection each time: the relay looks the caller up once per connection, and these tests change who calls
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	res, err := client.Post(h.srv.URL+"/driver/"+path+"?token=test-secret", "application/json", bytes.NewReader(b))
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func snapshot(value string) map[string]any {
	return map[string]any{"url": "https://github.com/login", "title": "Sign in", "ready": "complete", "key": "k1", "text": "Sign in " + value,
		"elements": []any{map[string]any{"node": 1, "role": "button", "label": "Sign in", "ops": []any{"click"}},
			map[string]any{"node": 2, "role": "password", "label": "Password", "ops": []any{"fill", "click"}}}}
}

func TestDriverWhoMayDrive(t *testing.T) {
	h := newHarness(t)
	driverChrome(h, func(string) any { return snapshot("") })
	if res, err := http.Post(h.srv.URL+"/driver/observe", "application/json", strings.NewReader("{}")); err != nil || res.StatusCode != 403 {
		t.Error("no secret:", res.StatusCode)
	}
	h.as(Caller{PID: 7, Command: "curl"}, false)
	if code, out := h.drive("observe", map[string]any{}); code != 403 || !strings.Contains(fmt.Sprint(out["error"]), "only accepts") {
		t.Error("not an agent app:", code, out)
	}
	a := h.agent("me@example.com") // claude, session "abc"
	h.as(Caller{PID: 4242, App: "claude", Cwd: "/work/my-repo", ClaudeSession: "abc"}, false)
	if code, out := h.drive("observe", map[string]any{}); code != 409 || !strings.Contains(fmt.Sprint(out["error"]), "no open tab of yours") {
		t.Error("no tab yet:", code, out)
	}
	_, tab := openTab(a, 1)
	if code, out := h.drive("observe", map[string]any{}); code != 200 || out["tab"] != float64(tab) || out["title"] != "Sign in" {
		t.Error("observe:", code, out)
	}
	h.as(Caller{PID: 5, App: "claude", Cwd: "/work/my-repo", ClaudeSession: "someone-else"}, false)
	if code, _ := h.drive("observe", map[string]any{"tab": tab}); code != 409 {
		t.Error("another agent's tab:", code)
	}
	h.as(Caller{PID: 4242, App: "claude", Cwd: "/work/my-repo", ClaudeSession: "abc", BrowserSession: "other"}, false)
	if code, _ := h.drive("observe", map[string]any{}); code != 409 {
		t.Error("another agent-browser session of the same agent:", code)
	}
	h.as(Caller{PID: 4242, App: "claude", Cwd: "/work/my-repo", ClaudeSession: "abc"}, false)
	if code, out := h.drive("observe", map[string]any{"tab": 9999}); code != 404 {
		t.Error("a tab id that isn't the agent's:", code, out)
	}
}

func TestDriverActsLikeTheAgent(t *testing.T) {
	h := newHarness(t)
	chrome := driverChrome(h, func(e string) any {
		switch {
		case strings.Contains(e, `"prepare"`):
			return map[string]any{"x": 10, "y": 20}
		case strings.Contains(e, `"readback"`):
			return map[string]any{"changed": true}
		case strings.Contains(e, `"check"`):
			return []any{true, false}
		case strings.Contains(e, `"settle"`):
			return true
		}
		return snapshot("hunter2-secret")
	})
	a := h.agent("me@example.com")
	sid, tab := openTab(a, 1)
	chrome.event(tab, "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "F", "url": "https://github.com/login"}})
	a.next(method("Page.frameNavigated"))
	h.as(Caller{PID: 4242, App: "claude", Cwd: "/work/my-repo", ClaudeSession: "abc"}, false)

	code, out := h.drive("act", map[string]any{"kind": "click", "node": 1, "label": "Sign in", "expect": map[string]any{"role": "button", "label": "Sign in"}})
	if code != 200 || out["error"] != nil || out["page"] == nil {
		t.Fatal("click:", code, out)
	}
	presses := chrome.all(func(m map[string]any) bool {
		p, _ := m["params"].(map[string]any)
		return m["method"] == "Input.dispatchMouseEvent" && p["type"] == "mousePressed" && p["x"] == 10.0
	})
	if len(presses) != 1 {
		t.Error("one real click at the checked point:", len(presses))
	}
	// a 1Password reference goes through the same path as the agent's own fill: typed from the vault, never shown
	code, out = h.drive("act", map[string]any{"kind": "fill", "node": 2, "label": "Password", "text": "op://Agents/GitHub/password"})
	if code != 200 || out["error"] != nil {
		t.Fatal("fill:", code, out)
	}
	typed := chrome.all(func(m map[string]any) bool { return m["method"] == "Input.insertText" })
	if len(typed) != 1 || typed[0]["params"].(map[string]any)["text"] != "hunter2-secret" {
		t.Error("typed from the vault:", typed)
	}
	if b, _ := json.Marshal(out); strings.Contains(string(b), "hunter2") {
		t.Error("the page read back shows the secret:", string(b))
	}
	code, out = h.drive("act", map[string]any{"kind": "fill", "node": 2, "label": "Password", "text": "op://Agents/Nope/password"})
	if !strings.Contains(fmt.Sprint(out["error"]), "1Password:") {
		t.Error("a refused reference says why:", code, out)
	}
	code, out = h.drive("check", map[string]any{"checks": []any{map[string]any{"kind": "text", "value": "Signed in"}, map[string]any{"kind": "url", "value": "/x"}}})
	if code != 200 || out["all"] != false {
		t.Error("check:", code, out)
	}
	h.drive("note", map[string]any{"event": "start", "detail": map[string]any{"goal": "sign in", "decider": "jev"}})
	// the agent's own session still works, and what it reads is masked too
	a.send(map[string]any{"id": 50, "sessionId": sid, "method": "Runtime.evaluate", "params": map[string]any{"expression": "document.title"}})
	a.next(id(50))
	time.Sleep(100 * time.Millisecond)
	b, _ := os.ReadFile(filepath.Join(h.home, "audit", time.Now().UTC().Format("2006-01-02")+".jsonl"))
	audit := string(b)
	for _, want := range []string{`"event":"click"`, `"event":"do click","tab":` + fmt.Sprint(tab) + `,"label":"Sign in"`, `"event":"1password fill"`,
		`"event":"do fill"`, `"event":"do start"`, `"goal":"sign in"`} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit lacks %s", want)
		}
	}
	if strings.Contains(audit, "hunter2") || strings.Contains(audit, "op://Agents/GitHub") && strings.Contains(audit, `"do fill","tab":`+fmt.Sprint(tab)+`,"label":"op://`) {
		t.Error("audit holds a secret or the reference as the label:", audit)
	}
}
