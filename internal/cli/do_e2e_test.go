package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/aindeev/agent-chrome-relay/internal/decide"
	"github.com/aindeev/agent-chrome-relay/internal/relay"
)

// End to end: `chrome-relay do` against real pages in a real (headless, throwaway) Chrome, through a real relay.
// A stand-in for the extension forwards the relay's DevTools commands to that Chrome. Opt in with
// CHROME_RELAY_E2E=1; CHROME_RELAY_E2E_JEV=1 (with TYPESAFE_API_KEY) or CHROME_RELAY_E2E_CLAUDE=1 also run the goals
// with the real deciders.

const e2eExtID = "abcdefghijklmnopabcdefghijklmnop"

var pages = map[string]string{
	"/form": `<title>Order</title><h1>Pizza order</h1><form action="/thanks">
<p><label>Customer name <input name="name" required></label></p>
<p><label>Telephone <input name="tel" type="tel"></label></p>
<p><label>E-mail address <input name="email" type="email"></label></p>
<p><label>Country <select name="country"><option>Choose…</option><option>Canada</option><option>France</option></select></label></p>
<fieldset><legend>Size</legend><label><input type="radio" name="size" value="small"> Small</label> <label><input type="radio" name="size" value="large"> Large</label></fieldset>
<p><label><input type="checkbox" name="news"> Subscribe to news</label></p>
<p><button>Submit order</button></p></form>`,
	"/search": `<title>Encyclopedia</title><h1>Encyclopedia</h1>
<input role="combobox" aria-label="Search the encyclopedia" aria-controls="list" id="q" autocomplete="off">
<ul role="listbox" id="list"></ul>
<script>
const items = ["Gödel's incompleteness theorems", "Gödel numbering", "Kurt Gödel"];
q.addEventListener('input', () => setTimeout(() => {
  list.innerHTML = '';
  for (const t of items.filter(t => q.value && t.toLowerCase().includes(q.value.toLowerCase().split(' ')[0]))) {
    const li = document.createElement('li'); li.setAttribute('role', 'option');
    const a = document.createElement('a'); a.href = '/article?t=' + encodeURIComponent(t); a.textContent = t; // as Wikipedia does
    li.append(a); list.append(li);
  }
}, 150));
</script>`,
	"/shadow": `<title>Projects</title><h1>New project</h1><proj-form></proj-form>
<script>
customElements.define('proj-form', class extends HTMLElement { connectedCallback() {
  const r = this.attachShadow({ mode: 'open' });
  r.innerHTML = '<label for="n">Project name</label> <input id="n"> <button>Create</button><p id="out"></p>';
  r.querySelector('button').onclick = () => r.getElementById('out').textContent = 'Created ' + r.getElementById('n').value;
} });
</script>`,
	"/danger": `<title>Invoices</title><h1>Invoices</h1><table>
<tr><td>Invoice 1</td><td><button onclick="location.href='/invoice'">Open</button></td><td><button onclick="document.title='deleted'">Delete</button></td></tr></table>`,
	"/invoice": `<title>Invoice 1</title><h1>Invoice 1</h1><p>Total 42</p>`,
	"/divs": `<title>Mail</title><h1>Mail</h1>
<div class="btn" style="cursor:pointer;display:inline-block;padding:8px" onclick="document.getElementById('out').textContent='Archived'">Archive</div>
<span tabindex="0" style="display:inline-block;padding:8px" id="star">Star</span>
<p>Just text, not clickable</p><p id="out"></p>
<table><tr><td>Message 1</td><td><button>Delete</button></td></tr><tr><td>Message 2</td><td><button>Delete</button></td></tr></table>
<script>star.addEventListener('click', () => out.textContent = 'Starred')</script>`,
	"/frames": `<title>Frames</title><h1>Outer page</h1><p>Above the frame</p>
<iframe src="/inner" width="500" height="160" style="border:3px solid #999;margin-left:40px"></iframe>
<iframe src="OTHER/inner" width="300" height="120"></iframe>`,
	"/inner":  `<title>Inner</title><label>Coupon code <input id="c"></label> <button onclick="document.getElementById('r').textContent='Applied '+c.value">Apply</button><p id="r"></p>`,
	"/upload": `<title>Upload</title><h1>Upload</h1><label class="b">Choose a file<input type="file" id="f" style="display:none" onchange="document.getElementById('n').textContent='Got '+this.files[0].name"></label><p id="n"></p>`,
	// a page that tries to steer the driver: it fakes where things are and plants a node table of its own
	"/hostile": `<title>Hostile</title><h1>Hostile</h1><button id="pay" style="position:absolute;top:10px;left:10px" onclick="out.textContent='PAID'">Pay now</button>
<button id="ok" style="position:absolute;top:200px;left:10px" onclick="out.textContent='OK'">Next</button><p id="out" style="position:absolute;top:300px"></p>
<script>const real = Element.prototype.getBoundingClientRect, pay = document.getElementById('pay');
Element.prototype.getBoundingClientRect = function () { return real.call(pay); };
document.elementFromPoint = () => document.getElementById('ok');
window.__crDrive = { nodes: new Map(), ids: new WeakMap(), next: 1, rect: () => real.call(pay), describe: () => ({}) };</script>`,
	// a list row with a control of its own inside, and a few odd ones
	"/rows": `<title>Rows</title><h1>Tasks</h1><ul><li id="row" role="button" tabindex="0" style="display:flex;width:400px;padding:4px" onclick="if (event.target === this || !event.target.closest('input')) out.textContent='opened'">
<input type="checkbox" aria-label="Done" onclick="event.stopPropagation(); out.textContent='toggled'"><span style="flex:1">Buy milk</span></li></ul>
<label><input type="checkbox" id="k"> Remember me</label><p id="out"></p><ol><li class="item">First item</li></ol>
<input type="search" aria-label="Search"><button>Search</button><search>not this</search>
<a href="/invoice">Next page</a>`,
	"/covered": `<title>Welcome</title><h1>Welcome</h1><button onclick="document.body.append('Continued')">Continue</button>
<div style="position:fixed;inset:0;background:rgba(0,0,0,.4)"><div style="background:#fff;margin:30vh auto;width:300px">Cookies? <button>Accept</button></div></div>`,
}

func site(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/thanks":
			fmt.Fprintf(w, "<title>Thanks</title><h1>Order received for %s</h1><p>%s</p>", r.URL.Query().Get("name"), r.URL.RawQuery)
		case "/article":
			fmt.Fprintf(w, "<title>%s</title><h1>%s</h1>", r.URL.Query().Get("t"), r.URL.Query().Get("t"))
		default:
			if p, ok := pages[r.URL.Path]; ok {
				// another origin for the same server: localhost instead of 127.0.0.1
				other := "http://localhost:" + r.Host[strings.LastIndex(r.Host, ":")+1:]
				fmt.Fprint(w, "<!doctype html><meta charset=utf-8>"+strings.ReplaceAll(p, "OTHER", other))
			} else {
				http.NotFound(w, r)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// cdpClient: one DevTools connection to the headless Chrome.
type cdpClient struct {
	ws      *websocket.Conn
	mu      sync.Mutex
	next    int
	pending map[int]chan map[string]any
	onEvent func(m map[string]any)
}

func (c *cdpClient) call(method string, params any, session string) (map[string]any, string) {
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan map[string]any, 1)
	c.pending[id] = ch
	msg := map[string]any{"id": id, "method": method, "params": params}
	if session != "" {
		msg["sessionId"] = session
	}
	err := c.ws.WriteJSON(msg)
	c.mu.Unlock()
	if err != nil {
		return nil, err.Error()
	}
	select {
	case m := <-ch:
		if e, ok := m["error"].(map[string]any); ok {
			return nil, fmt.Sprint(e["message"])
		}
		r, _ := m["result"].(map[string]any)
		return r, ""
	case <-time.After(30 * time.Second):
		return nil, "timeout"
	}
}

func startChrome(t *testing.T) *cdpClient {
	bin := os.Getenv("CHROME_BIN")
	if bin == "" {
		bin = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("no Chrome at", bin)
	}
	// not t.TempDir(): Chrome's helper processes may still be writing the profile when the test ends
	dir, err := os.MkdirTemp("", "chrome-relay-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--headless=new", "--remote-debugging-port=0", "--user-data-dir="+dir, "--no-first-run",
		"--no-default-browser-check", "--window-size=1200,900", "about:blank")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // Chrome and its helpers
		cmd.Wait()
		for range 20 {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	var lines []string
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(filepath.Join(dir, "DevToolsActivePort")); err == nil {
			if lines = strings.Split(strings.TrimSpace(string(b)), "\n"); len(lines) == 2 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(lines) != 2 {
		t.Fatal("Chrome didn't start")
	}
	ws, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+lines[0]+lines[1], nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &cdpClient{ws: ws, pending: map[int]chan map[string]any{}}
	go func() {
		for {
			var m map[string]any
			if ws.ReadJSON(&m) != nil {
				return
			}
			if id, ok := m["id"].(float64); ok {
				c.mu.Lock()
				ch := c.pending[int(id)]
				delete(c.pending, int(id))
				c.mu.Unlock()
				if ch != nil {
					ch <- m
				}
			} else if c.onEvent != nil {
				c.onEvent(m)
			}
		}
	}()
	t.Cleanup(func() { ws.Close() })
	return c
}

// fakeExtension: what the Chrome Relay extension does for the relay, done over DevTools on the headless Chrome.
func fakeExtension(t *testing.T, chrome *cdpClient, relayURL, profile string) {
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(relayURL, "http")+"/extension?profile="+url.QueryEscape(profile)+"&client=e2e",
		http.Header{"Origin": {"chrome-extension://" + e2eExtID}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	var mu sync.Mutex
	tabs, sessions, nextTab := map[int]string{}, map[string]int{}, 100
	send := func(v any) { mu.Lock(); defer mu.Unlock(); ws.WriteJSON(v) }
	chrome.onEvent = func(m map[string]any) {
		sid, _ := m["sessionId"].(string)
		mu.Lock()
		tab, ok := sessions[sid]
		mu.Unlock()
		if ok {
			send(map[string]any{"event": map[string]any{"tabId": tab, "method": m["method"], "params": m["params"]}})
		}
	}
	go func() {
		for {
			var m map[string]any
			if ws.ReadJSON(&m) != nil {
				return
			}
			go func(m map[string]any) {
				reply := map[string]any{"id": m["id"], "result": map[string]any{}}
				switch m["op"] {
				case "create":
					r, e := chrome.call("Target.createTarget", map[string]any{"url": "about:blank", "background": true}, "")
					if e != "" {
						reply = map[string]any{"id": m["id"], "error": e}
						break
					}
					a, e := chrome.call("Target.attachToTarget", map[string]any{"targetId": r["targetId"], "flatten": true}, "")
					if e != "" {
						reply = map[string]any{"id": m["id"], "error": e}
						break
					}
					mu.Lock()
					nextTab++
					tab := nextTab
					tabs[tab] = fmt.Sprint(a["sessionId"])
					sessions[fmt.Sprint(a["sessionId"])] = tab
					mu.Unlock()
					reply["result"] = map[string]any{"tabId": tab}
				case "close":
					tab := int(m["tabId"].(float64))
					mu.Lock()
					sid := tabs[tab]
					delete(tabs, tab)
					delete(sessions, sid)
					mu.Unlock()
					if sid != "" {
						if info, e := chrome.call("Target.getTargetInfo", map[string]any{}, sid); e == "" {
							if ti, ok := info["targetInfo"].(map[string]any); ok {
								chrome.call("Target.closeTarget", map[string]any{"targetId": ti["targetId"]}, "")
							}
						}
					}
				case "cdp":
					tab := int(m["tabId"].(float64))
					mu.Lock()
					sid := tabs[tab]
					mu.Unlock()
					r, e := chrome.call(fmt.Sprint(m["method"]), m["params"], sid)
					if e != "" {
						reply = map[string]any{"id": m["id"], "error": e}
					} else {
						reply["result"] = r
					}
				}
				send(reply)
			}(m)
		}
	}()
}

type e2e struct {
	t      *testing.T
	home   string
	port   int
	agent  *websocket.Conn
	tab    string
	site   *httptest.Server
	mu     sync.Mutex
	nextID int
}

// newE2E: a relay, a headless Chrome and the test pages; with an agent connection that opened a tab (as
// agent-browser would), unless own is set: then `chrome-relay open` starts the session.
func newE2E(t *testing.T, own ...bool) *e2e {
	if os.Getenv("CHROME_RELAY_E2E") != "1" {
		t.Skip("set CHROME_RELAY_E2E=1 to drive a headless Chrome")
	}
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "token"), []byte("test-secret"), 0o600)
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"extensionIds":["`+e2eExtID+`"]}`), 0o600)
	os.WriteFile(filepath.Join(home, "identities.json"), []byte(`{"profiles":{},"sites":{"rules":{},"learned":{}}}`), 0o600)
	who := relay.Caller{PID: 4242, App: "claude", Cwd: "/work", ClaudeSession: "e2e"}
	s, err := relay.New(relay.Options{Home: home, Port: 1, Caller: func(int) relay.Caller { return who }, IsChrome: func(int) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	port, _ := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	chrome := startChrome(t)
	fakeExtension(t, chrome, srv.URL, "me@example.com")
	time.Sleep(300 * time.Millisecond)
	if len(own) > 0 && own[0] {
		return &e2e{t: t, home: home, port: port, site: site(t)}
	}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/devtools/browser/agent?token=test-secret&profile=me%40example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	e := &e2e{t: t, home: home, port: port, agent: ws, site: site(t)}
	r := e.cmd("Target.createTarget", map[string]any{"url": "about:blank"}, "")
	e.tab = fmt.Sprint(r["targetId"])
	return e
}

// cmd: one DevTools command as the agent (agent-browser) would send it.
func (e *e2e) cmd(method string, params any, session string) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nextID++
	id := e.nextID
	m := map[string]any{"id": id, "method": method, "params": params}
	if session != "" {
		m["sessionId"] = session
	}
	e.agent.WriteJSON(m)
	for {
		var got map[string]any
		e.agent.SetReadDeadline(time.Now().Add(30 * time.Second))
		if err := e.agent.ReadJSON(&got); err != nil {
			e.t.Fatal(method, err)
		}
		if got["id"] == float64(id) {
			if er, ok := got["error"]; ok {
				e.t.Fatal(method, er)
			}
			r, _ := got["result"].(map[string]any)
			return r
		}
	}
}

func (e *e2e) open(path string) {
	e.cmd("Page.navigate", map[string]any{"url": e.site.URL + path}, e.tab)
	time.Sleep(400 * time.Millisecond)
}

// do runs `chrome-relay do` with these arguments and deciders; returns its output and exit code.
func (e *e2e) do(models []decide.Model, args ...string) (string, int) {
	var out bytes.Buffer
	names := []string{}
	for _, m := range models {
		names = append(names, m.Name())
	}
	if len(names) == 0 {
		names = []string{"none"}
	}
	c := &cli{instance: instance{Name: "chrome-relay", Home: e.home, Port: e.port}, args: append([]string{"chrome-relay", "do"}, args...),
		out: &out, errw: &out, models: func(string) ([]decide.Model, []string) { return models, names }}
	code := 0
	if err := c.run(); err != nil {
		code = 1
		if ec, ok := err.(exitCode); ok {
			code = ec.code
		}
		out.WriteString(err.Error())
	}
	return out.String(), code
}

// run any chrome-relay command; returns its output and exit code.
func (e *e2e) run(args ...string) (string, int) {
	var out bytes.Buffer
	c := &cli{instance: instance{Name: "chrome-relay", Home: e.home, Port: e.port}, args: append([]string{"chrome-relay"}, args...),
		out: &out, errw: &out, models: func(string) ([]decide.Model, []string) { return nil, []string{"none"} }}
	code := 0
	if err := c.run(); err != nil {
		code = 1
		if ec, ok := err.(exitCode); ok {
			code = ec.code
		}
		out.WriteString(err.Error())
	}
	return out.String(), code
}

// scripted: a stand-in decision model that picks by label, counting how often it's asked.
type scripted struct {
	calls int
	pick  func(r *decide.Request) (op, label, value string)
}

func (s *scripted) Name() string { return "scripted" }
func (s *scripted) Close()       {}
func (s *scripted) Ask(_ context.Context, r *decide.Request) (map[string]decide.Answer, error) {
	s.calls++
	op, label, value := s.pick(r)
	ans := map[string]decide.Answer{"operation": {Choice: op, Confidence: 0.9}}
	for i, e := range r.Candidates() {
		if strings.Contains(e.Label, label) && label != "" {
			idx := strconv.Itoa(i + 1)
			ans["click_target"] = decide.Answer{Choice: idx, Confidence: 0.9}
			ans["type_text_target"] = decide.Answer{Choice: idx, Confidence: 0.9}
			break
		}
	}
	if value != "" {
		ans["type_text_value"] = decide.Answer{Choice: value, Confidence: 0.9}
	}
	return ans, nil
}

func TestDoFormFlowAndReplay(t *testing.T) {
	e := newE2E(t)
	e.open("/form")
	m := &scripted{pick: func(r *decide.Request) (string, string, string) {
		for _, el := range r.Candidates() {
			if el.Label == "Large" && el.Checked != "true" {
				return "CLICK", "Large", ""
			}
		}
		return "CLICK", "Submit order", ""
	}}
	out, code := e.do([]decide.Model{m}, "Order a large pizza for the customer and submit the order",
		"--data", "Customer name=Ada Lovelace", "--data", "Telephone=555-0100", "--data", "E-mail address=ada@example.com",
		"--data", "Country=Canada", "--data", "Subscribe=yes", "--until", "text:Order received for Ada Lovelace")
	t.Log(out)
	if code != 0 || !strings.Contains(out, "DONE") {
		t.Fatalf("first run: code %d", code)
	}
	for _, want := range []string{"fill textbox \"Customer name\"", "select \"Canada\"", "click checkbox \"Subscribe to news\"", "(form", "(scripted"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if m.calls != 2 {
		t.Errorf("model asked %d times, want 2 (size, submit)", m.calls)
	}
	if !strings.Contains(out, "saved as a flow") {
		t.Error("no flow saved")
	}
	// the same kind of goal again, other data: the flow replays, no model at all
	e.open("/form")
	m2 := &scripted{pick: func(*decide.Request) (string, string, string) { return "BLOCKED", "", "" }}
	out, code = e.do([]decide.Model{m2}, "Order a large pizza for the customer and submit the order",
		"--data", "Customer name=Grace Hopper", "--data", "Telephone=555-0199", "--data", "E-mail address=grace@example.com",
		"--data", "Country=France", "--data", "Subscribe=no", "--until", "text:Order received for Grace Hopper")
	t.Log(out)
	if code != 0 || m2.calls != 0 || !strings.Contains(out, "(flow") {
		t.Errorf("replay: code %d, model calls %d", code, m2.calls)
	}
	if !strings.Contains(out, "country=France") && !strings.Contains(out, "France") {
		t.Error("France not chosen")
	}
}

func TestDoSearchSuggestionAndShadowDOM(t *testing.T) {
	e := newE2E(t)
	e.open("/search")
	m := &scripted{pick: func(r *decide.Request) (string, string, string) {
		for _, el := range r.Candidates() {
			if el.Role == "option" && strings.Contains(el.Label, "incompleteness") {
				return "CLICK", "incompleteness", ""
			}
		}
		return "TYPE_TEXT", "Search the encyclopedia", "quoted1"
	}}
	out, code := e.do([]decide.Model{m}, `Open the article on "Gödel incompleteness"`, "--until", "title:incompleteness theorems")
	t.Log(out)
	if code != 0 {
		t.Fatalf("search: code %d", code)
	}
	if strings.Count(out, "click") != 1 || strings.Contains(out, `= "0"`) {
		t.Error("a suggestion and the link inside it are one choice")
	}
	e.open("/shadow")
	out, code = e.do([]decide.Model{&scripted{pick: func(*decide.Request) (string, string, string) { return "CLICK", "Create", "" }}},
		"Create a project", "--data", "Project name=Apollo", "--until", "text:Created Apollo")
	t.Log(out)
	if code != 0 || !strings.Contains(out, `fill textbox "Project name"`) {
		t.Fatalf("shadow DOM: code %d", code)
	}
}

func TestDoHandsBack(t *testing.T) {
	e := newE2E(t)
	e.open("/danger")
	out, code := e.do([]decide.Model{&scripted{pick: func(*decide.Request) (string, string, string) { return "CLICK", "Delete", "" }}},
		"Open invoice 1", "--until", "title:Invoice 1")
	t.Log(out)
	if code != 2 || !strings.Contains(out, "doesn't say to delete") {
		t.Errorf("an unrequested Delete should come back: code %d", code)
	}
	e.open("/covered")
	out, code = e.do([]decide.Model{&scripted{pick: func(*decide.Request) (string, string, string) { return "CLICK", "Continue", "" }}},
		"Continue to the site", "--until", "text:Continued")
	t.Log(out)
	if code != 2 || !strings.Contains(out, "covers it") {
		t.Errorf("a covered button should come back: code %d", code)
	}
	// what was just typed doesn't count as the page saying it
	e.open("/form")
	out, code = e.do(nil, "Order for the customer", "--data", "Customer name=Ada Lovelace", "--until", "text:Ada Lovelace")
	t.Log(out)
	if code != 2 || strings.Contains(out, "DONE") {
		t.Errorf("a typed value met --until: code %d", code)
	}
	e.open("/invoice")
	out, code = e.do(nil, "Pay the invoice")
	t.Log(out)
	if code != 2 || !strings.Contains(out, "no decider") || !strings.Contains(out, "Invoice 1") {
		t.Errorf("no decider: code %d", code)
	}
}

// Real deciders on the same pages (opt in; they cost a little and need a key or Claude Code).
func TestDoRealDeciders(t *testing.T) {
	var models []decide.Model
	if os.Getenv("CHROME_RELAY_E2E_JEV") == "1" && os.Getenv("TYPESAFE_API_KEY") != "" {
		models = append(models, &decide.Jev{Key: os.Getenv("TYPESAFE_API_KEY")})
	}
	if os.Getenv("CHROME_RELAY_E2E_CLAUDE") == "1" {
		if p := claudePath(); p != "" {
			models = append(models, &decide.Claude{Path: p})
		}
	}
	if len(models) == 0 {
		t.Skip("set CHROME_RELAY_E2E_JEV=1 (with TYPESAFE_API_KEY) or CHROME_RELAY_E2E_CLAUDE=1")
	}
	e := newE2E(t)
	for _, m := range models {
		t.Run(m.Name(), func(t *testing.T) {
			defer m.Close()
			e.open("/form")
			out, code := e.do([]decide.Model{m}, "Order a large pizza for the customer and submit the order",
				"--data", "Customer name=Ada Lovelace", "--data", "Telephone=555-0100", "--data", "E-mail address=ada@example.com",
				"--until", "text:Order received for Ada Lovelace", "--no-flow")
			t.Log(out)
			if code != 0 {
				t.Errorf("form: code %d", code)
			}
			e.open("/search")
			out, code = e.do([]decide.Model{m}, `Open the encyclopedia article about "Gödel incompleteness theorems"`, "--until", "title:incompleteness theorems", "--no-flow")
			t.Log(out)
			if code != 0 {
				t.Errorf("search: code %d", code)
			}
			e.open("/danger")
			out, code = e.do([]decide.Model{m}, "Open invoice 1", "--until", "title:Invoice 1", "--no-flow")
			t.Log(out)
			if code != 0 {
				t.Errorf("invoice: code %d", code)
			}
		})
	}
}

// The step-by-step commands, with no agent-browser anywhere: `chrome-relay open` starts the session.
func TestDriveCommands(t *testing.T) {
	e := newE2E(t, true)
	want := func(out string, code int, wantCode int, subs ...string) {
		t.Helper()
		if code != wantCode {
			t.Errorf("exit %d, want %d: %s", code, wantCode, out)
		}
		for _, s := range subs {
			if !strings.Contains(out, s) {
				t.Errorf("missing %q in: %s", s, out)
			}
		}
	}
	run := func(args ...string) (string, int) {
		out, code := e.run(args...)
		t.Log(strings.Join(args, " "), "\n", out)
		return out, code
	}
	out, code := run("snapshot")
	want(out, code, 1, "no open tab of yours")
	out, code = run("open", e.site.URL+"/form")
	want(out, code, 0, "✓ Order")
	out, code = run("snapshot")
	want(out, code, 0, `- textbox "Customer name" [@e`, `- combobox "Country" [@e`, "(options: Choose… | Canada | France)", `- checkbox "Subscribe to news"`, `- button "Submit order"`, "required")
	out, code = run("fill", "Customer name", "Ada Lovelace")
	want(out, code, 0, `✓ fill textbox "Customer name"`)
	ref := regexp.MustCompile(`"Telephone" \[(@e\d+)\]`).FindStringSubmatch(func() string { o, _ := e.run("snapshot"); return o }())
	if ref == nil {
		t.Fatal("no ref for Telephone")
	}
	out, code = run("fill", ref[1], "555-0100")
	want(out, code, 0, `✓ fill textbox "Telephone"`)
	out, code = run("select", "Country", "France")
	want(out, code, 0, "✓ select")
	out, code = run("click", "radio:Large")
	want(out, code, 0, "(the page changed)")
	out, code = run("click", "Subscribe to news", "--snapshot")
	want(out, code, 0, `- checkbox "Subscribe to news" [@e`, "checked")
	out, code = run("get", "value", "Customer name")
	want(out, code, 0, "Ada Lovelace")
	out, code = run("click", "Nope")
	want(out, code, 1, `no control named "Nope"`)
	out, code = run("click", "Submit order")
	want(out, code, 0, "→ Thanks ·", "country=France", "size=large")
	out, code = run("wait", "text:Order received for Ada Lovelace")
	want(out, code, 0, "✓")
	out, code = run("wait", "text:never there", "--timeout", "0.4")
	want(out, code, 1, "still not true")
	out, code = run("get", "text", "h1")
	want(out, code, 0, "Order received for Ada Lovelace")
	out, code = run("get", "title")
	want(out, code, 0, "Thanks")
	out, code = run("eval", "document.querySelectorAll('h1').length + 1")
	want(out, code, 0, "2")
	out, code = run("eval", "nope.nope")
	want(out, code, 1, "ReferenceError")
	out, code = run("back")
	want(out, code, 0, "→ Order")
	shot := filepath.Join(t.TempDir(), "shot.png")
	out, code = run("screenshot", shot, "--full")
	want(out, code, 0, "✓")
	if b, err := os.ReadFile(shot); err != nil || len(b) < 2000 || string(b[1:4]) != "PNG" {
		t.Error("screenshot:", err, len(b))
	}
	// clickable things without a role, and never guessing between two controls
	out, code = run("open", e.site.URL+"/divs", "--new")
	want(out, code, 0, "✓ Mail")
	out, code = run("snapshot")
	want(out, code, 0, `- clickable "Archive" [@e`, `- clickable "Star" [@e`)
	if strings.Contains(out, "Just text") {
		t.Error("plain text listed as a control")
	}
	out, code = run("click", "Archive")
	want(out, code, 0, "(the page changed)")
	out, code = run("get", "text", "--selector", "#out")
	want(out, code, 0, "Archived")
	out, code = run("click", "Delete")
	want(out, code, 1, `"Delete" matches 2 controls; use a ref`)
	out, code = run("tabs")
	want(out, code, 0, "/form", "/divs")
	// a same-origin frame is read and driven; another site's frame is named
	out, code = run("open", e.site.URL+"/frames")
	want(out, code, 0, "✓ Frames")
	time.Sleep(300 * time.Millisecond)
	out, code = run("snapshot")
	want(out, code, 0, `- textbox "Coupon code" [@e`, "in-frame", "frames from other sites, which this can't read: localhost:")
	out, code = run("fill", "Coupon code", "SAVE10")
	want(out, code, 0, "✓ fill")
	out, code = run("click", "Apply")
	want(out, code, 0, "(the page changed)")
	out, code = run("wait", "text:Applied SAVE10", "--timeout", "3")
	want(out, code, 0, "✓")
	// a file field hidden behind its label
	out, code = run("open", e.site.URL+"/upload")
	want(out, code, 0, "✓ Upload")
	file := filepath.Join(t.TempDir(), "report.csv")
	os.WriteFile(file, []byte("a,b\n1,2\n"), 0o600)
	out, code = run("upload", "file:", file)
	if code != 0 {
		out, code = run("upload", "Choose a file", file)
	}
	want(out, code, 0, "✓ upload")
	out, code = run("wait", "text:Got report.csv", "--timeout", "3")
	want(out, code, 0, "✓")
	// the page's own scripts can't steer where a click lands
	out, code = run("open", e.site.URL+"/hostile")
	want(out, code, 0, "✓ Hostile")
	out, code = run("click", "Next")
	want(out, code, 0, "✓ click")
	out, code = run("get", "text", "--selector", "#out")
	want(out, code, 0, "OK")
	// a row's own control is not the row; Space; reading a list item; an ambiguous label is never taken as CSS
	out, code = run("open", e.site.URL+"/rows")
	want(out, code, 0, "✓ Rows")
	out, code = run("click", "button:Done Buy milk")
	want(out, code, 0, "✓ click")
	out, code = run("get", "text", "--selector", "#out")
	want(out, code, 0, "opened") // not "toggled": the row's checkbox is a control of its own
	out, code = run("get", "text", "li.item")
	want(out, code, 0, "First item")
	out, code = run("click", "Search")
	want(out, code, 1, `"Search" matches 2 controls`)
	out, code = run("click", "Remember")
	want(out, code, 1, `no control named exactly "Remember"`, "use its ref")
	out, code = run("click", "checkbox:Remember me")
	want(out, code, 0, "✓ click")
	out, code = run("press", "Space")
	want(out, code, 0, "✓")
	out, code = run("is", "checked", "Remember me")
	want(out, code, 0, "false")
	// a ref from a page that's gone never names a control on the next
	snap, _ := e.run("snapshot")
	old := regexp.MustCompile(`"Next page" \[(@e\d+)\]`).FindStringSubmatch(snap)
	if old == nil {
		t.Fatal("no ref for Next page")
	}
	out, code = run("click", old[1])
	want(out, code, 0, "→ Invoice 1")
	for _, n := range []string{old[1], "@e1", "@e2", "@e3"} {
		if out, code = run("click", n); code == 0 {
			t.Errorf("a ref from the old page clicked something on the new one: %s %s", n, out)
		}
	}
	// a file field by CSS selector
	out, code = run("open", e.site.URL+"/upload")
	want(out, code, 0, "✓ Upload")
	out, code = run("upload", "#f", file)
	want(out, code, 0, "✓ upload")
	// a covered button is refused, with what covers it
	out, code = run("open", e.site.URL+"/covered")
	want(out, code, 0, "✓ Welcome")
	out, code = run("click", "Continue")
	want(out, code, 1, "did nothing", "something covers it")
	// a whole goal, opening the page itself
	out, code = e.do([]decide.Model{&scripted{pick: func(*decide.Request) (string, string, string) { return "CLICK", "Create", "" }}},
		"Create a project", "--url", e.site.URL+"/shadow", "--data", "Project name=Apollo", "--until", "text:Created Apollo")
	t.Log(out)
	want(out, code, 0, "DONE")
	out, code = run("close", "--all")
	want(out, code, 0, "✓ closed 2 tab(s), 0 left")
	out, code = run("snapshot")
	want(out, code, 1, "no open tab of yours")
	b, _ := os.ReadFile(filepath.Join(e.home, "audit", time.Now().UTC().Format("2006-01-02")+".jsonl"))
	for _, ev := range []string{`"event":"session start"`, `"driver":true`, `"event":"tab open"`, `"event":"navigate"`, `"event":"do fill"`, `"event":"do click"`,
		`"event":"script"`, `"event":"screenshot"`, `"event":"upload"`, `"event":"session end"`} {
		if !strings.Contains(string(b), ev) {
			t.Errorf("audit lacks %s", ev)
		}
	}
	if strings.Contains(string(b), "Ada Lovelace") || strings.Contains(string(b), "SAVE10") {
		t.Error("the audit holds typed text")
	}
}
