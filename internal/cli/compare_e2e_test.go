package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
)

// Head to head: agent-browser and chrome-relay's own commands, through the same relay into the same headless
// Chrome, on the same pages. Each scenario's page tells the test server when the action really happened, so a
// tool's "done" is never taken on its word. Opt in: CHROME_RELAY_COMPARE=1 (needs agent-browser on PATH; the
// coverage part needs the network). It reports; it only fails where chrome-relay's own commands fall short of
// what the README claims.

var marks sync.Map

const markJS = `<script>const mark = (k, v) => fetch('/mark?k=' + k + '&v=' + encodeURIComponent(v ?? 'yes'));</script>`

var scenarioPages = map[string]string{
	"/s-form":    `<title>Form</title><label>City <input id="c"></label> <button onclick="mark('form', c.value)">Go</button>`,
	"/s-covered": `<title>Covered</title><button onclick="mark('covered')">Continue</button><div style="position:fixed;inset:0;background:rgba(0,0,0,.4)"><button onclick="mark('overlay')">Accept cookies</button></div>`,
	"/s-shadow": `<title>Shadow</title><x-form></x-form><script>customElements.define('x-form', class extends HTMLElement { connectedCallback() {
  const r = this.attachShadow({ mode: 'open' }); r.innerHTML = '<label for="n">Project name</label> <input id="n"> <button>Create</button>';
  r.querySelector('button').onclick = () => mark('shadow', r.getElementById('n').value); } });</script>`,
	"/s-frame":  `<title>Frame</title><h1>Outer</h1><iframe src="/s-inner?k=frame" width="500" height="120"></iframe>`,
	"/s-xframe": `<title>Cross frame</title><h1>Outer</h1><iframe src="OTHER/s-inner?k=xframe" width="500" height="120"></iframe>`,
	"/s-inner":  `<title>Inner</title><label>Coupon code <input id="c"></label> <button onclick="mark(new URLSearchParams(location.search).get('k'), c.value)">Apply</button>`,
	"/s-div":    `<title>Div</title><div style="cursor:pointer;padding:10px;display:inline-block" id="a">Archive</div><script>a.addEventListener('click', () => mark('div'))</script>`,
	"/s-upload": `<title>Upload</title><label>Choose a file<input type="file" style="display:none" onchange="mark('upload', this.files[0].name)"></label>`,
	"/s-select": `<title>Select</title><label>Country <select onchange="mark('select', this.value)"><option>Choose</option><option>Canada</option><option>France</option></select></label>`,
	"/s-suggest": `<title>Suggest</title><input role="combobox" aria-label="Search" aria-controls="l" id="q" autocomplete="off"><ul role="listbox" id="l"></ul>
<script>q.addEventListener('input', () => setTimeout(() => { l.innerHTML = ''; for (const t of ['Lisbon', 'Lima', 'Lille'].filter(t => q.value && t.toLowerCase().startsWith(q.value.toLowerCase()))) {
  const li = document.createElement('li'); li.setAttribute('role', 'option'); li.textContent = t; li.onclick = () => mark('suggest', t); l.append(li); } }, 200));</script>`,
	"/s-alert":   `<title>Alert</title><button onclick="alert('Heads up'); mark('alert')">Notify</button>`,
	"/s-confirm": `<title>Confirm</title><button onclick="mark('confirm', confirm('Really delete?') ? 'accepted' : 'dismissed')">Delete item</button>`,
	"/s-blank":   `<title>Blank</title><a href="/s-newtab" target="_blank">Open report</a>`,
	"/s-newtab":  `<title>Report page</title><h1>Report</h1><script>mark('blank')</script>`,
	"/s-delayed": `<title>Delayed</title><div id="h"></div><script>setTimeout(() => { h.innerHTML = '<button onclick="mark(\'delayed\')">Late button</button>'; }, 1200)</script>`,
	"/s-rerender": `<title>Rerender</title><ul id="u"></ul><script>const draw = () => { u.innerHTML = ''; for (const t of ['Item A', 'Item B', 'Item C']) {
  const b = document.createElement('button'); b.textContent = t; b.onclick = () => mark('rerender', t); const li = document.createElement('li'); li.append(b); u.append(li); } }; draw(); setInterval(draw, 2500);</script>`,
	"/s-check": `<title>Check</title><label><input type="checkbox" id="k" checked> Keep me signed in</label> <button onclick="mark('check', k.checked)">Save</button>`,
	"/s-enter": `<title>Enter</title><form onsubmit="mark('enter', q.value); return false"><input id="q" aria-label="Query"></form>`,
	"/s-hover": `<title>Hover</title><div id="m" style="padding:10px;display:inline-block">Account menu</div><div id="p" hidden><button onclick="mark('hover')">Settings</button></div><script>m.addEventListener('mouseenter', () => p.hidden = false)</script>`,
	"/s-drag": `<title>Drag</title><div id="a" draggable="true" style="width:80px;height:40px;background:#9cf">Card</div><div id="b" style="width:200px;height:100px;margin-top:40px;background:#eee">Done column</div>
<script>a.addEventListener('dragstart', e => e.dataTransfer.setData('text', 'card')); b.addEventListener('dragover', e => e.preventDefault()); b.addEventListener('drop', e => { e.preventDefault(); mark('drag'); });</script>`,
	"/s-editable":  `<title>Editable</title><div contenteditable="true" aria-label="Note" id="n" style="border:1px solid #999;min-height:40px"></div><button onclick="mark('editable', n.innerText)">Save note</button>`,
	"/s-offscreen": `<title>Offscreen</title><div style="height:3000px">tall</div><button onclick="mark('offscreen')">Bottom button</button>`,
	"/s-radio": `<title>Radio</title><style>input[type=radio]{position:absolute;opacity:0;width:0;height:0}label{display:inline-block;padding:8px;border:1px solid #999}</style>
<input type="radio" name="s" id="r1" value="standard"><label for="r1">Standard</label><input type="radio" name="s" id="r2" value="express" onchange="mark('radio', this.value)"><label for="r2">Express</label>`,
	"/s-imglink":  `<title>Image link</title><a href="/s-newtab?x" onclick="mark('imglink'); return false"><img alt="Fork me" style="position:absolute;top:0;right:0;width:120px;height:120px;background:#333"></a><p>Text</p>`,
	"/s-dblclick": `<title>Dblclick</title><button ondblclick="mark('dblclick')">Rename</button>`,
}

func compareSite(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mark" {
			marks.Store(r.URL.Query().Get("k"), r.URL.Query().Get("v"))
			w.Header().Set("Access-Control-Allow-Origin", "*")
			return
		}
		p, ok := scenarioPages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		other := "http://localhost:" + r.Host[strings.LastIndex(r.Host, ":")+1:]
		fmt.Fprint(w, "<!doctype html><meta charset=utf-8>"+markJS+strings.ReplaceAll(p, "OTHER", other))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func marked(k string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for {
		if v, ok := marks.Load(k); ok {
			return fmt.Sprint(v)
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// tool: one of the two CLIs, run as a real process.
type tool struct {
	name string
	run  func(timeout time.Duration, args ...string) (out string, ok bool, took time.Duration)
}

func execTool(env []string, bin string, pre ...string) func(time.Duration, ...string) (string, bool, time.Duration) {
	return func(timeout time.Duration, args ...string) (string, bool, time.Duration) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, append(append([]string{}, pre...), args...)...)
		cmd.Env = env
		start := time.Now()
		b, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			return string(b) + "\n(timed out after " + timeout.String() + ")", false, time.Since(start)
		}
		return string(b), err == nil, time.Since(start)
	}
}

// abRef finds agent-browser's ref for a control by (part of) its name, from `snapshot -i --json`.
func abRef(ab tool, name string, roles ...string) string {
	out, _, _ := ab.run(20*time.Second, "snapshot", "-i", "--json")
	var snap struct {
		Data struct {
			Refs map[string]struct{ Name, Role string } `json:"refs"`
		} `json:"data"`
	}
	json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &snap)
	keys := make([]string, 0, len(snap.Data.Refs))
	for k := range snap.Data.Refs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r := snap.Data.Refs[k]
		if !strings.Contains(strings.ToLower(r.Name), strings.ToLower(name)) {
			continue
		}
		if len(roles) > 0 && !strings.Contains(strings.Join(roles, ","), r.Role) {
			continue
		}
		return "@" + k
	}
	return "@none-" + strings.ReplaceAll(name, " ", "-")
}

type scenario struct {
	name, path, key, want string
	wantHonestFailure     bool // the right outcome is the tool saying it could not
	ab, cr                func(run func(args ...string) (string, bool), ref func(name string, roles ...string) string, file string) (string, bool)
}

func TestCompareWithAgentBrowser(t *testing.T) {
	if os.Getenv("CHROME_RELAY_COMPARE") != "1" {
		t.Skip("set CHROME_RELAY_COMPARE=1 (and CHROME_RELAY_E2E=1) to compare with agent-browser")
	}
	abBin, err := exec.LookPath("agent-browser")
	if err != nil {
		t.Skip("agent-browser is not installed")
	}
	e := newE2E(t, true)
	site := compareSite(t)
	bin := filepath.Join(t.TempDir(), "chrome-relay")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/chrome-relay")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatal(string(b))
	}
	env := append(os.Environ(), "CHROME_RELAY_HOME="+e.home, fmt.Sprintf("CHROME_RELAY_PORT=%d", e.port), "AGENT_BROWSER_SESSION=compare-"+fmt.Sprint(os.Getpid()),
		"CHROME_RELAY_SESSION=")
	cr := tool{"chrome-relay", execTool(env, bin)}
	ab := tool{"agent-browser", execTool(env, abBin)}
	cdp := fmt.Sprintf("ws://127.0.0.1:%d/devtools/browser/agent?token=test-secret&profile=me%%40example.com", e.port)
	file := filepath.Join(t.TempDir(), "report.csv")
	os.WriteFile(file, []byte("a,b\n1,2\n"), 0o600)

	type fn = func(run func(args ...string) (string, bool), ref func(name string, roles ...string) string, file string) (string, bool)
	seq := func(steps ...[]string) fn { // run the steps in order; "@name" arguments are looked up as refs
		return func(run func(args ...string) (string, bool), ref func(string, ...string) string, file string) (string, bool) {
			var out string
			ok := true
			for _, s := range steps {
				args := make([]string, len(s))
				for i, a := range s {
					switch {
					case strings.HasPrefix(a, "@"):
						args[i] = ref(a[1:])
					case a == "FILE":
						args[i] = file
					default:
						args[i] = a
					}
				}
				o, k := run(args...)
				out += o
				ok = ok && k
			}
			return out, ok
		}
	}
	scenarios := []scenario{
		{name: "fill a field, click a button", path: "/s-form", key: "form", want: "Lisbon",
			ab: seq([]string{"fill", "@City", "Lisbon"}, []string{"click", "@Go"}), cr: seq([]string{"fill", "City", "Lisbon"}, []string{"click", "Go"})},
		{name: "a button covered by an overlay (should say so)", path: "/s-covered", key: "covered", wantHonestFailure: true,
			ab: seq([]string{"click", "@Continue"}), cr: seq([]string{"click", "Continue"})},
		{name: "controls inside shadow DOM", path: "/s-shadow", key: "shadow", want: "Apollo",
			ab: seq([]string{"fill", "@Project name", "Apollo"}, []string{"click", "@Create"}), cr: seq([]string{"fill", "Project name", "Apollo"}, []string{"click", "Create"})},
		{name: "controls in a same-site frame", path: "/s-frame", key: "frame", want: "SAVE10",
			ab: seq([]string{"fill", "@Coupon", "SAVE10"}, []string{"click", "@Apply"}), cr: seq([]string{"fill", "Coupon code", "SAVE10"}, []string{"click", "Apply"})},
		{name: "controls in another site's frame", path: "/s-xframe", key: "xframe", want: "SAVE10",
			ab: seq([]string{"fill", "@Coupon", "SAVE10"}, []string{"click", "@Apply"}), cr: seq([]string{"fill", "Coupon code", "SAVE10"}, []string{"click", "Apply"})},
		{name: "a div that acts as a button", path: "/s-div", key: "div", want: "yes",
			ab: seq([]string{"click", "@Archive"}), cr: seq([]string{"click", "Archive"})},
		{name: "a radio hidden behind its styled label", path: "/s-radio", key: "radio", want: "express",
			ab: seq([]string{"click", "@Express"}), cr: seq([]string{"click", "Express"})},
		{name: "a link that is only a positioned image", path: "/s-imglink", key: "imglink", want: "yes",
			ab: seq([]string{"click", "@Fork me"}), cr: seq([]string{"click", "Fork me"})},
		{name: "upload through a hidden file field", path: "/s-upload", key: "upload", want: "report.csv",
			ab: seq([]string{"upload", "input[type=file]", "FILE"}), cr: seq([]string{"upload", "Choose a file", "FILE"})},
		{name: "choose a dropdown option", path: "/s-select", key: "select", want: "France",
			ab: seq([]string{"select", "@Country", "France"}), cr: seq([]string{"select", "Country", "France"})},
		{name: "type, then pick a suggestion", path: "/s-suggest", key: "suggest", want: "Lisbon",
			ab: seq([]string{"fill", "@Search", "Lis"}, []string{"wait", "600"}, []string{"click", "@Lisbon"}), cr: seq([]string{"fill", "Search", "Lis"}, []string{"click", "Lisbon"})},
		{name: "a click that raises an alert", path: "/s-alert", key: "alert", want: "yes",
			ab: seq([]string{"click", "@Notify"}), cr: seq([]string{"click", "Notify"})},
		{name: "accept a confirm dialog", path: "/s-confirm", key: "confirm", want: "accepted",
			ab: seq([]string{"dialog", "accept"}, []string{"click", "@Delete item"}, []string{"dialog", "accept"}), cr: seq([]string{"dialog", "accept"}, []string{"click", "Delete item"})},
		{name: "a link that opens a new tab", path: "/s-blank", key: "blank", want: "yes",
			ab: seq([]string{"click", "@Open report"}), cr: seq([]string{"click", "Open report"})},
		{name: "a button that appears after a second", path: "/s-delayed", key: "delayed", want: "yes",
			ab: seq([]string{"wait", "1600"}, []string{"click", "@Late button"}), cr: seq([]string{"wait", "selector:button"}, []string{"click", "Late button"})},
		{name: "a list the page keeps redrawing", path: "/s-rerender", key: "rerender", want: "Item B",
			ab: func(run func(args ...string) (string, bool), ref func(string, ...string) string, _ string) (string, bool) {
				r := ref("Item B")
				time.Sleep(2800 * time.Millisecond) // the page has replaced the button since the snapshot
				return run("click", r)
			},
			cr: func(run func(args ...string) (string, bool), _ func(string, ...string) string, _ string) (string, bool) {
				out, _ := run("snapshot")
				r := regexp.MustCompile(`"Item B" \[(@e\d+)\]`).FindStringSubmatch(out)
				time.Sleep(2800 * time.Millisecond)
				if r == nil {
					return out, false
				}
				if o, ok := run("click", r[1]); ok { // the old ref
					return o, true
				}
				return run("click", "Item B") // an agent's next move: by name
			}},
		{name: "check a box that is already checked", path: "/s-check", key: "check", want: "true",
			ab: seq([]string{"check", "@Keep me"}, []string{"click", "@Save"}), cr: seq([]string{"check", "Keep me signed in"}, []string{"click", "Save"})},
		{name: "press Enter to submit", path: "/s-enter", key: "enter", want: "hello",
			ab: seq([]string{"fill", "@Query", "hello"}, []string{"press", "Enter"}), cr: seq([]string{"fill", "Query", "hello"}, []string{"press", "Enter"})},
		{name: "hover to reveal a menu, click in it", path: "/s-hover", key: "hover", want: "yes",
			ab: seq([]string{"hover", "#m"}, []string{"click", "@Settings"}), cr: seq([]string{"hover", "#m"}, []string{"click", "Settings"})},
		{name: "drag and drop", path: "/s-drag", key: "drag", want: "yes",
			ab: seq([]string{"drag", "#a", "#b"}), cr: seq([]string{"drag", "#a", "#b"})},
		{name: "type into a rich-text box", path: "/s-editable", key: "editable", want: "Remember the milk",
			ab: seq([]string{"fill", "@Note", "Remember the milk"}, []string{"click", "@Save note"}), cr: seq([]string{"fill", "Note", "Remember the milk"}, []string{"click", "Save note"})},
		{name: "a button far down the page", path: "/s-offscreen", key: "offscreen", want: "yes",
			ab: seq([]string{"click", "@Bottom button"}), cr: seq([]string{"click", "Bottom button"})},
		{name: "double-click", path: "/s-dblclick", key: "dblclick", want: "yes",
			ab: seq([]string{"dblclick", "@Rename"}), cr: seq([]string{"dblclick", "Rename"})},
	}

	type result struct {
		ok   bool
		note string
	}
	results := map[string]map[string]result{"agent-browser": {}, "chrome-relay": {}}
	play := func(tl tool, sc scenario, open []string) {
		marks.Delete(sc.key)
		marks.Delete("overlay")
		run := func(args ...string) (string, bool) { o, ok, _ := tl.run(25*time.Second, args...); return o, ok }
		if o, ok := run(append(open, site.URL+sc.path)...); !ok {
			results[tl.name][sc.name] = result{false, "could not open: " + firstLine(o)}
			return
		}
		time.Sleep(250 * time.Millisecond)
		f := sc.cr
		ref := func(string, ...string) string { return "" }
		if tl.name == "agent-browser" {
			f = sc.ab
			ref = func(name string, roles ...string) string { return abRef(tl, name, roles...) }
		}
		out, said := f(run, ref, file)
		got := marked(sc.key, 1500*time.Millisecond)
		switch {
		case sc.wantHonestFailure:
			_, overlay := marks.Load("overlay")
			switch {
			case got != "":
				results[tl.name][sc.name] = result{true, "clicked through (the page did get the click)"}
			case !said:
				results[tl.name][sc.name] = result{true, "refused: " + firstLine(out)}
			default:
				results[tl.name][sc.name] = result{false, fmt.Sprintf("said it clicked; nothing happened (overlay clicked: %v)", overlay)}
			}
		case got == sc.want:
			results[tl.name][sc.name] = result{true, ""}
		default:
			results[tl.name][sc.name] = result{false, fmt.Sprintf("page got %q, want %q; tool said: %s", got, sc.want, firstLine(out))}
		}
	}
	// chrome-relay's own commands first, then agent-browser: one session at a time in the relay
	for _, sc := range scenarios {
		play(cr, sc, []string{"open"})
	}
	// timings: the same five commands, five times each
	timing := map[string]map[string][]time.Duration{"agent-browser": {}, "chrome-relay": {}}
	measure := func(tl tool, label string, args ...string) {
		_, _, d := tl.run(25*time.Second, args...)
		timing[tl.name][label] = append(timing[tl.name][label], d)
	}
	for i := 0; i < 5; i++ {
		measure(cr, "open a page", "open", site.URL+"/s-form")
		measure(cr, "snapshot", "snapshot")
		measure(cr, "fill", "fill", "City", "Lisbon")
		measure(cr, "click", "click", "Go")
		measure(cr, "read text", "get", "text")
	}
	// what each sees on real pages
	sites := []string{"https://en.wikipedia.org/wiki/Main_Page", "https://github.com/vercel-labs/agent-browser", "https://news.ycombinator.com/",
		"https://httpbin.org/forms/post", "https://developer.mozilla.org/en-US/", "https://docs.python.org/3/", "https://www.npmjs.com/package/express",
		"https://duckduckgo.com/", "https://the-internet.herokuapp.com/", "https://stackoverflow.com/questions"}
	if os.Getenv("CHROME_RELAY_COMPARE_OFFLINE") == "1" {
		sites = nil
	}
	if only := os.Getenv("CHROME_RELAY_COMPARE_SITES"); only != "" {
		sites = strings.Fields(only)
	}
	if os.Getenv("CHROME_RELAY_COMPARE_SKIP_SCENARIOS") == "1" {
		scenarios = nil
	}
	// a bot check that passes by itself ("Just a moment..."): both get the same time for it
	passCheck := func(d tool) {
		for i := 0; i < 15; i++ {
			if title, _, _ := d.run(10*time.Second, "get", "title"); !strings.Contains(title, "Just a moment") {
				return
			}
			time.Sleep(time.Second)
		}
	}
	norm := func(s string) string {
		return strings.ToLower(strings.TrimRight(strings.Join(strings.Fields(s), " "), ": "))
	}
	seen := map[string]map[string]map[string]bool{"agent-browser": {}, "chrome-relay": {}}
	interactive := map[string]bool{}
	for _, r := range strings.Fields("button link textbox searchbox combobox checkbox radio switch tab menuitem menuitemcheckbox menuitemradio option slider spinbutton treeitem listbox clickable") {
		interactive[r] = true
	}
	for _, u := range sites {
		opened, ok, _ := cr.run(40*time.Second, "open", u)
		if os.Getenv("CHROME_RELAY_COMPARE_DEBUG") == "1" {
			t.Logf("open %s ok=%v: %.600s", u, ok, opened)
		}
		if !ok {
			continue
		}
		time.Sleep(800 * time.Millisecond)
		passCheck(cr)
		out, _, _ := cr.run(30*time.Second, "snapshot", "--json")
		if os.Getenv("CHROME_RELAY_COMPARE_DEBUG") == "1" {
			t.Logf("snapshot %s: %.1500s", u, out)
		}
		var p struct {
			Elements []struct {
				Label, Role string
				Options     []string
			}
		}
		if i := strings.Index(out, "{"); i >= 0 {
			json.Unmarshal([]byte(out[i:]), &p)
		}
		seen["chrome-relay"][u] = map[string]bool{}
		for _, el := range p.Elements {
			seen["chrome-relay"][u][norm(el.Label)] = true
			for _, o := range el.Options { // agent-browser lists a dropdown's options as controls of their own
				seen["chrome-relay"][u][norm(o)] = true
			}
		}
	}
	cr.run(20*time.Second, "close", "--all")

	ab.run(40*time.Second, "--cdp", cdp, "open", site.URL+"/s-form")
	for _, sc := range scenarios {
		play(ab, sc, []string{"open"})
	}
	for i := 0; i < 5; i++ {
		measure(ab, "open a page", "open", site.URL+"/s-form")
		measure(ab, "snapshot", "snapshot", "-i")
		measure(ab, "fill", "fill", abRef(ab, "City"), "Lisbon")
		measure(ab, "click", "click", abRef(ab, "Go"))
		measure(ab, "read text", "get", "text", "body")
	}
	for _, u := range sites {
		_, ok, _ := ab.run(40*time.Second, "open", u)
		if !ok {
			continue
		}
		time.Sleep(800 * time.Millisecond)
		passCheck(ab)
		out, _, _ := ab.run(30*time.Second, "snapshot", "-i", "--json")
		var snap struct {
			Data struct {
				Refs map[string]struct{ Name, Role string } `json:"refs"`
			} `json:"data"`
		}
		if i := strings.Index(out, "{"); i >= 0 {
			json.Unmarshal([]byte(out[i:]), &snap)
		}
		seen["agent-browser"][u] = map[string]bool{}
		for _, r := range snap.Data.Refs {
			if interactive[r.Role] { // agent-browser also lists named headings, cells and list items: not controls
				seen["agent-browser"][u][norm(r.Name)] = true
			}
		}
	}
	ab.run(20*time.Second, "close")

	var b strings.Builder
	b.WriteString("\n## Scenarios (the page confirms the action happened)\n\n| Scenario | agent-browser | chrome-relay |\n|---|---|---|\n")
	wins := map[string]int{}
	cell := func(r result) string {
		if r.ok {
			return strings.TrimSpace("✓ " + r.note)
		}
		return "✗ " + r.note
	}
	for _, sc := range scenarios {
		a, c := results["agent-browser"][sc.name], results["chrome-relay"][sc.name]
		if a.ok {
			wins["agent-browser"]++
		}
		if c.ok {
			wins["chrome-relay"]++
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", sc.name, cell(a), cell(c))
	}
	fmt.Fprintf(&b, "| **passed** | **%d of %d** | **%d of %d** |\n", wins["agent-browser"], len(scenarios), wins["chrome-relay"], len(scenarios))
	median := func(d []time.Duration) time.Duration {
		if len(d) == 0 {
			return 0
		}
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[len(d)/2]
	}
	b.WriteString("\n## Time per command (median of 5, a real process each time)\n\n| Command | agent-browser | chrome-relay |\n|---|---|---|\n")
	for _, l := range []string{"open a page", "snapshot", "fill", "click", "read text"} {
		fmt.Fprintf(&b, "| %s | %d ms | %d ms |\n", l, median(timing["agent-browser"][l]).Milliseconds(), median(timing["chrome-relay"][l]).Milliseconds())
	}
	if len(sites) > 0 {
		b.WriteString("\n## Controls each lists on real pages (by name; buttons, links, fields and the like)\n\n| Page | both | only agent-browser | only chrome-relay |\n|---|---|---|---|\n")
		for _, u := range sites {
			a, c := seen["agent-browser"][u], seen["chrome-relay"][u]
			if a == nil || c == nil {
				fmt.Fprintf(&b, "| %s | (one of them could not open it) | | |\n", u)
				continue
			}
			// the same control under a slightly different name counts as the same: one tool names a card by its
			// heading, the other by all its text; one writes "0.1%", the other "0.1 %"
			bare := func(s string) string {
				return strings.Map(func(r rune) rune {
					if unicode.IsLetter(r) || unicode.IsDigit(r) {
						return r
					}
					return -1
				}, s)
			}
			has := func(set map[string]bool, k string) bool {
				if set[k] {
					return true
				}
				kb := bare(k)
				for o := range set {
					ob := bare(o)
					if ob == kb || (len(kb) >= 4 && len(ob) >= 4 && (strings.Contains(ob, kb) || strings.Contains(kb, ob))) {
						return true
					}
				}
				return false
			}
			both, onlyA, onlyC := 0, []string{}, []string{}
			for k := range a {
				if k == "" {
					continue
				}
				if has(c, k) {
					both++
				} else {
					onlyA = append(onlyA, k)
				}
			}
			for k := range c {
				if k != "" && !has(a, k) {
					onlyC = append(onlyC, k)
				}
			}
			sort.Strings(onlyA)
			sort.Strings(onlyC)
			sample := func(l []string) string {
				if len(l) > 4 {
					l = l[:4]
				}
				for i := range l {
					if len(l[i]) > 28 {
						l[i] = l[i][:28] + "…"
					}
				}
				return strings.Join(l, "; ")
			}
			fmt.Fprintf(&b, "| %s | %d | %d (%s) | %d (%s) |\n", strings.TrimPrefix(u, "https://"), both, len(onlyA), sample(onlyA), len(onlyC), sample(onlyC))
		}
	}
	if out := os.Getenv("CHROME_RELAY_COMPARE_OUT"); out != "" {
		dump, _ := json.MarshalIndent(seen, "", " ")
		os.WriteFile(out+".seen.json", dump, 0o644)
	}
	t.Log(b.String())
	if out := os.Getenv("CHROME_RELAY_COMPARE_OUT"); out != "" {
		os.WriteFile(out, []byte(b.String()), 0o644)
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 110 {
		s = s[:110] + "…"
	}
	return s
}
