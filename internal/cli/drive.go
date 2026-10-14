package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aindeev/agent-chrome-relay/internal/decide"
)

const driveUsage = `Drive a background tab of your Chrome, one step at a time (for a whole goal, ` + "`chrome-relay do`" + `):

  chrome-relay open <url> [--profile <email>] [--new]   open a page (the first one starts your session)
  chrome-relay snapshot [--text] [--json]               the page's controls, each with a ref like @e5
  chrome-relay click <target> [--snapshot]              <target>: @e5, the control's label ("Save", button:Save),
                                                        or a CSS selector (#save, css:main button)
  chrome-relay fill <target> <text>                     replace a field's text (text may be op://vault/item/field)
  chrome-relay select <target> <option>                 choose a dropdown option
  chrome-relay type <target> <text>   add to a field       chrome-relay check | uncheck <target>
  chrome-relay hover | dblclick <target>                chrome-relay upload <target> <file>...
  chrome-relay drag <target> <onto target>              chrome-relay is visible|enabled|checked <target>
  chrome-relay dialog accept [text] | dismiss           how to answer the next confirm or prompt (alerts are accepted;
                                                        an unanswered confirm is dismissed, never left open)
  chrome-relay press <key>        Enter, Tab, Escape, Space, Backspace, Delete, Arrow*, Home, End, PageUp, PageDown
  chrome-relay scroll [up|down] [px]   chrome-relay back   chrome-relay reload
  chrome-relay get text [<target> | --selector <css>]   chrome-relay get url | title | value <target>
  chrome-relay get html <target> | attr <name> <target> | count <css>
  chrome-relay console [--clear]  chrome-relay errors   what the page logged and threw since the tab opened
  chrome-relay cookies [--values] chrome-relay pdf [file.pdf]
  chrome-relay cdp <Domain.method> ['<json>']           any other DevTools command in your tab (emulation, network, ...)
  chrome-relay eval <javascript>                        run your own script in the page, print its value
  chrome-relay screenshot [file.png] [--full]
  chrome-relay wait <check> [--timeout <seconds>]       text:<s> (default), no-text:, url:, title:, selector:, js:, or milliseconds
  chrome-relay tabs               chrome-relay tab <id>            chrome-relay close [--all]

Every action checks the control first (still there, enabled, on screen, not covered), waits for the page to settle and
says what changed. Refs last until the page navigates. One session per agent; name several with CHROME_RELAY_SESSION.`

// flags pulls --name [value] options out of args: bools are flags without a value.
func flags(args []string, bools ...string) (rest []string, opt map[string]string) {
	opt = map[string]string{}
	isBool := map[string]bool{}
	for _, b := range bools {
		isBool[b] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") || len(a) < 3 {
			rest = append(rest, a)
			continue
		}
		name := a[2:]
		if isBool[name] {
			opt[name] = "true"
		} else if i+1 < len(args) {
			i++
			opt[name] = args[i]
		}
	}
	return rest, opt
}

func (c *cli) newDriver() (*driver, error) {
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{MaxIdleConnsPerHost: 1, MaxConnsPerHost: 1, IdleConnTimeout: 5 * time.Minute}
	// a fill from 1Password waits for the person's approval (Touch ID, or their iPhone): allow for it
	return &driver{c: c, tok: tok, client: &http.Client{Transport: tr, Timeout: 6 * time.Minute}}, nil
}

var refRe = regexp.MustCompile(`^@?e(\d+)$`)

// selectorRe: what looks like a CSS selector rather than a label (#id, .class, [attr], tag#id, tag.class, css:...).
var selectorRe = regexp.MustCompile(`^(css:|[#.\[]|[a-z][a-z0-9-]*([#.\[>:]|$))`)

// asSelector: the CSS selector an agent meant, if what it gave isn't a control's label.
func asSelector(what string) (string, bool) {
	if !selectorRe.MatchString(what) || strings.Contains(what, " ") && !strings.ContainsAny(what, "#.[>") {
		return "", false
	}
	return strings.TrimPrefix(what, "css:"), true
}

// noControl: nothing on the page has that label (as opposed to several having it).
type noControl struct{ msg string }

func (e noControl) Error() string { return e.msg }

// target finds the control an agent means: a ref from the last snapshot (@e5), or a label, optionally with its
// role (button:Save). A label must match exactly one control: it never guesses between two. Part of a label is
// enough to fill or read, but not to click: "Cancel" never clicks "Cancel subscription".
func target(page decide.Page, what, op string) (decide.Element, error) {
	if m := refRe.FindStringSubmatch(what); m != nil {
		n, _ := strconv.Atoi(m[1])
		if e, ok := page.Element(n); ok {
			return e, nil
		}
		return decide.Element{}, noControl{fmt.Sprintf("no control @e%d on this page: take a new snapshot (refs don't carry over to a new page)", n)}
	}
	role, label := "", what
	if r, l, ok := strings.Cut(what, ":"); ok && regexp.MustCompile(`^[a-z]+$`).MatchString(r) && l != "" {
		role, label = r, l
	}
	norm := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	var exact, partial []decide.Element
	for _, e := range page.Elements {
		if role != "" && e.Role != role {
			continue
		}
		if op != "" && !e.Can(op) {
			continue
		}
		switch l := norm(e.Label); {
		case l == norm(label):
			exact = append(exact, e)
		case strings.Contains(l, norm(label)):
			partial = append(partial, e)
		}
	}
	hits := exact
	if len(hits) == 0 {
		hits = partial
		if op == "click" && len(hits) == 1 {
			e := hits[0]
			return decide.Element{}, fmt.Errorf("no control named exactly %q; to click %s %q, use its ref @e%d", what, e.Role, cut(e.Label, 60), e.Node)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return decide.Element{}, noControl{fmt.Sprintf("no control named %q on this page (`chrome-relay snapshot` lists them)", what)}
	}
	var names []string
	for i, e := range hits {
		if i == 6 {
			names = append(names, "…")
			break
		}
		names = append(names, fmt.Sprintf("@e%d %s %q", e.Node, e.Role, cut(e.Label, 50)))
	}
	return decide.Element{}, fmt.Errorf("%q matches %d controls; use a ref: %s", what, len(hits), strings.Join(names, ", "))
}

func elementLine(e decide.Element) string {
	s := fmt.Sprintf("- %s %q [@e%d]", e.Role, e.Label, e.Node)
	switch {
	case e.Role == "password" && e.Filled:
		s += " (filled)"
	case e.Value != "" && e.Role != "radio" && e.Role != "checkbox":
		s += fmt.Sprintf(" = %q", cut(e.Value, 80))
	}
	if e.Checked == "true" {
		s += " checked"
	} else if e.Checked == "false" && (e.Role == "checkbox" || e.Role == "radio" || e.Role == "switch") {
		s += " unchecked"
	}
	if e.Selected == "true" {
		s += " selected"
	}
	if e.Expanded != "" {
		s += " expanded=" + e.Expanded
	}
	if len(e.Options) > 0 {
		opts := e.Options
		if len(opts) > 12 {
			opts = append(append([]string{}, opts[:12]...), "…")
		}
		s += " (options: " + strings.Join(opts, " | ") + ")"
	}
	if e.Required {
		s += " required"
	}
	if e.Frame {
		s += " in-frame"
	}
	return s
}

func (c *cli) printPage(p decide.Page, text bool) {
	c.printf("%s · %s\n", p.Title, p.URL)
	if p.Dialog != "" {
		c.printf("open dialog: %s\n", cut(p.Dialog, 120))
	}
	for _, e := range p.Elements {
		c.println(elementLine(e))
	}
	if p.Omitted > 0 {
		c.printf("(%d more controls further down the page)\n", p.Omitted)
	}
	if len(p.OtherFrames) > 0 {
		c.printf("(frames from other sites, which this can't read: %s)\n", strings.Join(p.OtherFrames, ", "))
	}
	if text {
		c.printf("\n%s\n", p.Text)
	}
}

// drive is every step-by-step command: open, snapshot, click, fill, ... (see driveUsage).
func (c *cli) drive(cmd string) error {
	rest, opt := flags(c.args[2:], "new", "all", "full", "text", "json", "snapshot", "clear", "values")
	arg := func(i int) string {
		if i < len(rest) {
			return rest[i]
		}
		return ""
	}
	if opt["help"] != "" || arg(0) == "-h" {
		return errors.New(driveUsage)
	}
	d, err := c.newDriver()
	if err != nil {
		return err
	}
	if t, _ := strconv.Atoi(opt["tab"]); t > 0 {
		d.tab = t
	}
	observe := func() (decide.Page, error) {
		var p decide.Page
		err := d.call("observe", map[string]any{"tab": d.tab, "limit": 2000}, &p)
		return p, err
	}
	// where: the control as the relay should find it: a node from the snapshot, or a CSS selector
	where := func(page decide.Page, what, need string) (map[string]any, string, error) {
		e, err := target(page, what, need)
		var none noControl
		if err == nil {
			return map[string]any{"node": e.Node, "label": e.Label, "expect": map[string]string{"role": e.Role, "label": e.Label}},
				fmt.Sprintf("%s %q [@e%d]", e.Role, cut(e.Label, 60), e.Node), nil
		}
		if sel, ok := asSelector(what); ok && !refRe.MatchString(what) && errors.As(err, &none) { // only when no label matched
			return map[string]any{"selector": sel, "label": sel}, sel, nil
		}
		return nil, "", err
	}
	// act on a control and say what happened
	act := func(kind, what string, body map[string]any, need string) error {
		page, err := observe()
		if err != nil {
			return err
		}
		w, shown, err := where(page, what, need)
		if err != nil {
			return err
		}
		for k, v := range w {
			body[k] = v
		}
		body["tab"], body["kind"], body["brief"] = d.tab, kind, opt["snapshot"] == ""
		if to, ok := body["dropOn"].(string); ok { // drag: the second element
			delete(body, "dropOn")
			w2, shown2, err := where(page, to, "")
			if err != nil {
				return err
			}
			body["to"], body["toSelector"] = w2["node"], w2["selector"]
			shown += " onto " + shown2
		}
		e := decide.Element{}
		if n, ok := w["node"].(int); ok {
			e, _ = page.Element(n)
		}
		var res actResult
		if err := d.call("act", body, &res); err != nil {
			return err
		}
		if res.Stale != "" || res.Refused != "" {
			return fmt.Errorf("%s %s did nothing: %s%s", kind, shown, res.Stale, res.Refused)
		}
		c.printf("✓ %s %s", kind, shown)
		for _, dlg := range res.Dialogs {
			c.printf(" [%s]", dlg)
		}
		switch {
		case res.Navigated:
			c.printf(" → %s · %s\n", res.Page.Title, res.Page.URL)
		case (kind == "fill") && !strings.HasPrefix(strings.TrimSpace(fmt.Sprint(body["text"])), "op://") && e.Role != "password" &&
			res.Readback.Value != nil && fmt.Sprint(res.Readback.Value) != fmt.Sprint(body["text"]):
			c.printf(" (the field now shows %q)\n", cut(fmt.Sprint(res.Readback.Value), 80))
		case res.Readback.Changed || res.Readback.Gone || res.Page.Key != page.Key:
			c.printf(" (the page changed)\n")
		case kind == "hover" || kind == "upload":
			c.printf("\n")
		default:
			c.printf(" (nothing on the page changed)\n")
		}
		if opt["snapshot"] != "" {
			c.printPage(res.Page, false)
		}
		return nil
	}
	simple := func(kind, text string) error {
		var res actResult
		if err := d.call("act", map[string]any{"tab": d.tab, "kind": kind, "text": text}, &res); err != nil {
			return err
		}
		c.printf("✓ %s %s→ %s · %s\n", kind, text+map[bool]string{true: " ", false: ""}[text != ""], res.Page.Title, res.Page.URL)
		if opt["snapshot"] != "" {
			c.printPage(res.Page, false)
		}
		return nil
	}
	switch cmd {
	case "open":
		if arg(0) == "" {
			return errors.New("usage: chrome-relay open <url> [--profile <email>] [--new]")
		}
		u := arg(0)
		if !strings.Contains(u, "://") && !strings.HasPrefix(u, "about:") {
			u = "https://" + u
		}
		var res struct {
			Profile string      `json:"profile"`
			Page    decide.Page `json:"page"`
		}
		if err := d.call("open", map[string]any{"text": u, "profile": opt["profile"], "new": opt["new"] != ""}, &res); err != nil {
			return err
		}
		c.printf("✓ %s · %s\n", res.Page.Title, res.Page.URL)
		if opt["snapshot"] != "" {
			c.printPage(res.Page, false)
		}
	case "snapshot":
		p, err := observe()
		if err != nil {
			return err
		}
		if opt["json"] != "" {
			b, _ := json.MarshalIndent(p, "", "  ")
			c.println(string(b))
			return nil
		}
		c.printPage(p, opt["text"] != "")
	case "click", "hover", "dblclick":
		if arg(0) == "" {
			return fmt.Errorf("usage: chrome-relay %s <target>", cmd)
		}
		need := "click"
		if cmd == "hover" {
			need = ""
		}
		return act(cmd, arg(0), map[string]any{}, need)
	case "drag":
		if len(rest) < 2 {
			return errors.New("usage: chrome-relay drag <target> <onto target>")
		}
		return act("drag", arg(0), map[string]any{"dropOn": arg(1)}, "")
	case "console", "errors":
		var res struct {
			Lines []string `json:"lines"`
		}
		if err := d.call("console", map[string]any{"tab": d.tab, "clear": opt["clear"] != ""}, &res); err != nil {
			return err
		}
		n := 0
		for _, l := range res.Lines {
			if cmd == "console" || strings.Contains(l, " error: ") {
				c.println(l)
				n++
			}
		}
		if n == 0 {
			c.printf("nothing (since this tab was opened%s)\n", map[bool]string{true: "; errors only", false: ""}[cmd == "errors"])
		}
	case "cookies":
		var res struct {
			Result struct {
				Cookies []struct {
					Name, Value, Domain, Path string
					HTTPOnly                  bool `json:"httpOnly"`
					Secure                    bool
					Session                   bool
				}
			} `json:"result"`
		}
		if err := d.call("cdp", map[string]any{"tab": d.tab, "name": "Network.getCookies"}, &res); err != nil {
			return err
		}
		for _, ck := range res.Result.Cookies {
			v := fmt.Sprintf("(%d chars)", len(ck.Value))
			if opt["values"] != "" {
				v = ck.Value
			}
			c.printf("%s=%s  %s%s%s%s\n", ck.Name, v, ck.Domain, ck.Path, map[bool]string{true: " httpOnly"}[ck.HTTPOnly], map[bool]string{true: " secure"}[ck.Secure])
		}
		if len(res.Result.Cookies) == 0 {
			c.println("no cookies for this page")
		}
	case "pdf":
		path := arg(0)
		if path == "" {
			path = fmt.Sprintf("chrome-relay-%s.pdf", time.Now().Format("20060102-150405"))
		}
		var res struct {
			Result struct {
				Data string `json:"data"`
			} `json:"result"`
		}
		if err := d.call("cdp", map[string]any{"tab": d.tab, "name": "Page.printToPDF", "params": map[string]any{"printBackground": true}}, &res); err != nil {
			return err
		}
		b, err := base64.StdEncoding.DecodeString(res.Result.Data)
		if err != nil || len(b) == 0 {
			return errors.New("Chrome returned no PDF")
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
		c.printf("✓ %s (%d KB)\n", path, len(b)/1024)
	case "cdp":
		if arg(0) == "" {
			return errors.New("usage: chrome-relay cdp <Domain.method> ['<json params>']   (any other DevTools command, in your tab)")
		}
		body := map[string]any{"tab": d.tab, "name": arg(0)}
		if arg(1) != "" {
			if !json.Valid([]byte(arg(1))) {
				return errors.New("the parameters are not valid JSON")
			}
			body["params"] = json.RawMessage(arg(1))
		}
		var res struct {
			Result json.RawMessage `json:"result"`
		}
		if err := d.call("cdp", body, &res); err != nil {
			return err
		}
		c.println(string(res.Result))
	case "is":
		if len(rest) < 2 || !map[string]bool{"visible": true, "enabled": true, "checked": true}[arg(0)] {
			return errors.New("usage: chrome-relay is visible|enabled|checked <target>")
		}
		page, err := observe()
		if err != nil {
			return err
		}
		w, _, err := where(page, arg(1), "")
		if err != nil {
			var none noControl
			if arg(0) == "visible" && errors.As(err, &none) { // snapshots leave out what can't be seen
				c.println("false")
				return nil
			}
			return err
		}
		var res struct {
			Value any `json:"value"`
		}
		if err := d.call("query", map[string]any{"tab": d.tab, "node": w["node"], "selector": w["selector"], "what": arg(0)}, &res); err != nil {
			if arg(0) == "visible" && strings.HasPrefix(err.Error(), "nothing matches") {
				c.println("false")
				return nil
			}
			return err
		}
		c.println(res.Value)
	case "check", "uncheck":
		if arg(0) == "" {
			return fmt.Errorf("usage: chrome-relay %s <target>", cmd)
		}
		page, err := observe()
		if err != nil {
			return err
		}
		e, err := target(page, arg(0), "click")
		if err != nil {
			return err
		}
		if e.Checked == "" {
			return fmt.Errorf("%s %q is not a checkbox, radio or switch", e.Role, cut(e.Label, 60))
		}
		if (e.Checked == "true") == (cmd == "check") {
			c.printf("✓ %s %q is already %sed\n", e.Role, cut(e.Label, 60), cmd)
			return nil
		}
		return act("click", fmt.Sprintf("@e%d", e.Node), map[string]any{}, "")
	case "type":
		if len(rest) < 2 {
			return errors.New("usage: chrome-relay type <target> <text>  (adds to the field; fill replaces)")
		}
		return act("type", arg(0), map[string]any{"text": arg(1)}, "fill")
	case "dialog":
		if arg(0) != "accept" && arg(0) != "dismiss" {
			return errors.New("usage: chrome-relay dialog accept [text] | dismiss   (how to answer the next confirm or prompt; alerts are accepted on their own)")
		}
		if err := d.call("dialog", map[string]any{"tab": d.tab, "kind": arg(0), "text": arg(1)}, nil); err != nil {
			return err
		}
		c.printf("✓ the next confirm or prompt will be %sed\n", arg(0))
	case "fill":
		if len(rest) < 2 {
			return errors.New("usage: chrome-relay fill <target> <text>")
		}
		return act("fill", arg(0), map[string]any{"text": arg(1)}, "fill")
	case "select":
		if len(rest) < 2 {
			return errors.New("usage: chrome-relay select <target> <option>")
		}
		return act("select", arg(0), map[string]any{"option": arg(1)}, "select")
	case "upload":
		if len(rest) < 2 {
			return errors.New("usage: chrome-relay upload <target> <file>...")
		}
		var files []string
		for _, f := range rest[1:] {
			abs, err := filepath.Abs(f)
			if err != nil {
				return err
			}
			if _, err := os.Stat(abs); err != nil {
				return fmt.Errorf("no such file: %s", f)
			}
			files = append(files, abs)
		}
		return act("upload", arg(0), map[string]any{"files": files}, "upload")
	case "press":
		if arg(0) == "" {
			return errors.New("usage: chrome-relay press <key>")
		}
		return simple("key", arg(0))
	case "scroll":
		var res actResult
		if err := d.call("act", map[string]any{"tab": d.tab, "kind": "scroll", "text": arg(0), "option": arg(1)}, &res); err != nil {
			return err
		}
		c.printf("✓ scroll %s→ at %d of %d px\n", arg(0)+map[bool]string{true: " "}[arg(0) != ""], res.Page.Scroll.Y, res.Page.Scroll.Height)
	case "back", "reload":
		return simple(cmd, "")
	case "get":
		switch arg(0) {
		case "url", "title":
			p, err := observe()
			if err != nil {
				return err
			}
			c.println(map[string]string{"url": p.URL, "title": p.Title}[arg(0)])
		case "html", "attr", "count":
			i := 1
			name := ""
			if arg(0) == "attr" {
				name, i = arg(1), 2
			}
			if arg(i) == "" || (arg(0) == "attr" && name == "") {
				return errors.New("usage: chrome-relay get html <target> | attr <name> <target> | count <css selector>")
			}
			page, err := observe()
			if err != nil {
				return err
			}
			body := map[string]any{"tab": d.tab, "what": arg(0), "name": name}
			if arg(0) == "count" {
				body["selector"] = strings.TrimPrefix(arg(i), "css:")
			} else {
				w, _, err := where(page, arg(i), "")
				if err != nil {
					return err
				}
				body["node"], body["selector"] = w["node"], w["selector"]
			}
			var res struct {
				Value any `json:"value"`
			}
			if err := d.call("query", body, &res); err != nil {
				return err
			}
			if res.Value == nil {
				c.println("")
			} else {
				c.println(res.Value)
			}
		case "text", "value":
			body := map[string]any{"tab": d.tab, "selector": opt["selector"]}
			if arg(1) != "" {
				page, err := observe()
				if err != nil {
					return err
				}
				e, err := target(page, arg(1), "")
				if err != nil {
					// agent-browser's habit: `get text main` is a selector (when no control has that label)
					var none noControl
					if !refRe.MatchString(arg(1)) && opt["selector"] == "" && errors.As(err, &none) {
						body["selector"] = arg(1)
					} else {
						return err
					}
				} else {
					body["node"] = e.Node
				}
			}
			var res struct {
				Text string `json:"text"`
			}
			if err := d.call("text", body, &res); err != nil {
				return err
			}
			c.println(strings.TrimRight(res.Text, "\n"))
		default:
			return errors.New("usage: chrome-relay get text [<target> | --selector <css>] | value <target> | html <target> | attr <name> <target> | count <css> | url | title")
		}
	case "eval":
		if arg(0) == "" {
			return errors.New("usage: chrome-relay eval <javascript>")
		}
		var res struct {
			Value       json.RawMessage `json:"value"`
			Type        string          `json:"type"`
			Description string          `json:"description"`
		}
		if err := d.call("eval", map[string]any{"tab": d.tab, "text": strings.Join(rest, " ")}, &res); err != nil {
			return err
		}
		var s string
		switch {
		case len(res.Value) == 0 || string(res.Value) == "null":
			c.println(cmpOr(res.Description, cmpOr(res.Type, "undefined")))
		case json.Unmarshal(res.Value, &s) == nil:
			c.println(s)
		default:
			c.println(string(res.Value))
		}
	case "screenshot":
		path := arg(0)
		if path == "" {
			path = fmt.Sprintf("chrome-relay-%s.png", time.Now().Format("20060102-150405"))
		}
		var res struct {
			PNG string `json:"png"`
		}
		if err := d.call("screenshot", map[string]any{"tab": d.tab, "full": opt["full"] != ""}, &res); err != nil {
			return err
		}
		b, err := base64.StdEncoding.DecodeString(res.PNG)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
		c.printf("✓ %s (%d KB)\n", path, len(b)/1024)
	case "wait":
		if arg(0) == "" {
			return errors.New("usage: chrome-relay wait <check> [--timeout <seconds>]  (text:<s>, no-text:, url:, title:, selector:, js:, or milliseconds)")
		}
		if ms, err := strconv.Atoi(arg(0)); err == nil {
			time.Sleep(time.Duration(ms) * time.Millisecond)
			return nil
		}
		kind, val, ok := strings.Cut(arg(0), ":")
		switch kind {
		case "text", "no-text", "url", "title", "selector", "js":
		default:
			kind, val, ok = "text", arg(0), true
		}
		if !ok || val == "" {
			return errors.New("wait: say what to wait for")
		}
		timeout := 30.0
		if t, err := strconv.ParseFloat(opt["timeout"], 64); err == nil && t > 0 {
			timeout = t
		}
		start := time.Now()
		for {
			var r struct {
				All    bool     `json:"all"`
				Errors []string `json:"errors"`
			}
			if err := d.call("check", map[string]any{"tab": d.tab, "checks": []map[string]string{{"kind": kind, "value": val}}}, &r); err != nil {
				return err
			}
			if len(r.Errors) > 0 {
				return errors.New(strings.Join(r.Errors, "; "))
			}
			if r.All {
				c.printf("✓ %s (%.1fs)\n", arg(0), time.Since(start).Seconds())
				return nil
			}
			if time.Since(start).Seconds() > timeout {
				return fmt.Errorf("still not true after %.0fs: %s", timeout, arg(0))
			}
			time.Sleep(100 * time.Millisecond)
		}
	case "tabs":
		var res struct {
			Profile string `json:"profile"`
			Tabs    []struct {
				Tab     int    `json:"tab"`
				URL     string `json:"url"`
				Current bool   `json:"current"`
			} `json:"tabs"`
		}
		if err := d.call("tabs", map[string]any{}, &res); err != nil {
			return err
		}
		for _, t := range res.Tabs {
			c.printf("%s %d  %s\n", map[bool]string{true: "*", false: " "}[t.Current], t.Tab, t.URL)
		}
	case "tab":
		t, err := strconv.Atoi(arg(0))
		if err != nil {
			return errors.New("usage: chrome-relay tab <id>  (ids: chrome-relay tabs)")
		}
		var res struct {
			URL string `json:"url"`
		}
		if err := d.call("tab", map[string]any{"tab": t}, &res); err != nil {
			return err
		}
		c.printf("✓ tab %d  %s\n", t, res.URL)
	case "close":
		var res struct {
			Closed, Left int
		}
		if err := d.call("close", map[string]any{"tab": d.tab, "all": opt["all"] != ""}, &res); err != nil {
			if strings.Contains(err.Error(), "no open tab of yours") {
				c.println("nothing open")
				return nil
			}
			return err
		}
		c.printf("✓ closed %d tab(s), %d left\n", res.Closed, res.Left)
	}
	return nil
}
