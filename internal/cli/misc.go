package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func (c *cli) url() error {
	if err := c.ensureUp(); err != nil {
		return err
	}
	email := c.arg(2)
	if email != "" {
		d, err := c.ids()
		if err != nil {
			return err
		}
		if !d.obj("profiles").has(email) {
			return fmt.Errorf("unknown profile %s; see `%s identities`", email, c.Name)
		}
	}
	tok, err := c.token()
	if err != nil {
		return err
	}
	c.println(agentURL(c.Port, tok, email))
	return nil
}

// agentURL: token first, then profile, as Python's urlencode did.
func agentURL(port int, token, email string) string {
	q := "token=" + url.QueryEscape(token)
	if email != "" {
		q += "&profile=" + url.QueryEscape(email)
	}
	return fmt.Sprintf("ws://127.0.0.1:%d/devtools/browser/agent?%s", port, q)
}

func (c *cli) status() error {
	if err := c.ensureUp(); err != nil {
		return err
	}
	body, err := c.getRaw("/status", 3*time.Second)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	if err := json.Indent(&b, bytes.TrimSpace(body), "", " "); err != nil { // keeps the relay's key order
		return err
	}
	c.println(b.String())
	return nil
}

func (c *cli) identities() error {
	d, err := c.ids()
	if err != nil {
		return err
	}
	var live []string
	if c.up() {
		if s, err := c.relayStatus(); err == nil {
			live = s.Profiles
		}
	}
	c.printf("Chrome profiles (pass the email to `%s url`):\n", c.Name)
	profiles := d.obj("profiles")
	for _, email := range profiles.keys {
		p := profiles.obj(email)
		state := "not connected"
		if slices.Contains(live, email) {
			state = "connected"
		}
		c.printf("  %s %s [%s]  %s\n", pyFormat(email, 30), pyFormat(orEmpty(p, "chromeProfile"), 10), state, pyStr(orEmpty(p, "use")))
		if accounts, ok := p.get("googleAccounts").([]any); ok && len(accounts) > 0 {
			names := make([]string, len(accounts))
			for i, a := range accounts {
				names[i] = pyStr(a)
			}
			c.printf("  %s Google accounts signed in there: %s\n", pyFormat("", 30), strings.Join(names, ", "))
		}
	}
	c.println("\nGoogle account per site (hand-set rules, then learned):")
	sites := newObject() // learned, overridden by rules (a rule keeps a learned site's place, like {**learned, **rules})
	for _, kind := range []string{"learned", "rules"} {
		o := d.obj("sites").obj(kind)
		for _, k := range o.keys {
			sites.set(k, o.get(k))
		}
	}
	for _, k := range sites.keys {
		c.printf("  %s %s\n", pyFormat(k, 36), pyStr(sites.get(k)))
	}
	return nil
}

// orEmpty is Python's p.get(k, "").
func orEmpty(o *object, k string) any {
	if !o.has(k) {
		return ""
	}
	return o.get(k)
}

// auditArgs: N is the first all-digit argument (default 40), the filter the first other one.
func auditArgs(args []string) (n int, filter string) {
	n = -1
	gotFilter := false
	for _, a := range args {
		if a != "" && strings.IndexFunc(a, func(r rune) bool { return !unicode.IsDigit(r) }) < 0 {
			if n < 0 {
				n, _ = strconv.Atoi(a)
			}
		} else if !gotFilter {
			filter, gotFilter = a, true
		}
	}
	if n < 0 {
		n = 40
	}
	return n, filter
}

func (c *cli) audit() error {
	n, filter := auditArgs(c.args[min(2, len(c.args)):])
	files, _ := filepath.Glob(filepath.Join(c.Home, "audit", "*.jsonl"))
	sort.Strings(files)
	if len(files) > 3 {
		files = files[len(files)-3:]
	}
	var lines []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		s := bufio.NewScanner(bytes.NewReader(b))
		s.Buffer(make([]byte, 1<<20), 64<<20)
		for s.Scan() {
			if l := s.Text(); strings.TrimSpace(l) != "" && strings.Contains(l, filter) {
				lines = append(lines, l)
			}
		}
	}
	if n > 0 && n < len(lines) { // Python's lines[-0:] is every line
		lines = lines[len(lines)-n:]
	}
	for _, l := range lines {
		if out, ok := auditLine(l); ok {
			c.println(out)
		}
	}
	return nil
}

// auditLine prints one audit event: time, session, app, profile, event, then the rest as k=v.
func auditLine(line string) (string, bool) {
	v, err := parseJSON([]byte(line))
	if err != nil {
		return "", false
	}
	e, ok := v.(*object)
	if !ok {
		return "", false
	}
	pop := func(k string) any {
		x := e.get(k)
		e.del(k)
		if x == nil {
			return ""
		}
		return x
	}
	t := []rune(pyStr(pop("t")))
	t = t[min(11, len(t)):min(19, len(t))]
	s, via, prof, ev := pop("s"), pop("via"), pop("profile"), pop("event")
	var rest []string
	for _, k := range e.keys {
		if x := e.get(k); x != nil {
			rest = append(rest, k+"="+pyStr(x))
		}
	}
	return fmt.Sprintf("%s %s %s %s %s ", string(t), pyStr(s), pyFormat(via, 6), pyFormat(prof, 26), pyFormat(ev, 22)) + strings.Join(rest, " "), true
}

func (c *cli) logs() error {
	n := c.arg(2)
	if n == "" {
		n = "30"
	}
	cmd := exec.Command("tail", "-n", n, filepath.Join(c.Home, "relay.log"))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Run() // tail says what's wrong itself
	return nil
}

func (c *cli) update() error {
	var busy int
	if c.up() {
		if s, err := c.relayStatus(); err == nil {
			busy = len(s.Agents)
		}
	}
	if busy > 0 && !hasFlag(c.args, "--force") {
		return fmt.Errorf("%d agent session(s) are using the relay; updating would cut them off. Retry later or add --force.", busy)
	}
	if b, err := os.ReadFile(c.Plist); err != nil || !strings.Contains(string(b), "<string>"+xmlEscape(c.Bin)+"</string>") {
		fmt.Fprintf(c.errw, "note: the background service does not run %s yet: `%s setup` switches it\n", c.Bin, c.Name)
	}
	c.kickstart(true)
	for range 180 { // the extension reconnects within ~30s (it retries on a 30s alarm)
		if c.up() {
			if s, err := c.relayStatus(); err == nil && len(s.Profiles) > 0 {
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	time.Sleep(2 * time.Second) // give other profiles a moment to reconnect too
	var r struct {
		Reloading []string `json:"reloading"`
	}
	if err := c.get("/reload-extensions", 3*time.Second, &r); err != nil {
		return fmt.Errorf("the relay did not come back (%v): run `%s doctor`", err, c.Name)
	}
	if len(r.Reloading) == 0 {
		c.printf("relay restarted; reloading extension in: no connected profiles yet: run `%s update` again in a minute\n", c.Name)
	} else {
		c.println("relay restarted; reloading extension in:", pyRepr(r.Reloading))
	}
	return nil
}
