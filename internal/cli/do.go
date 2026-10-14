package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aindeev/agent-chrome-relay/internal/decide"
	"github.com/aindeev/agent-chrome-relay/internal/macos"
)

const doUsage = `usage: chrome-relay do "<goal>" [--until <check>]... [--data <key>=<value>]... [options]

Carries out a goal in your current tab (` + "`chrome-relay open <url>`" + ` first, or pass --url):
fills what --data names, replays what worked before on this site, and asks a fast decision model for the rest.
Whatever it isn't sure of comes back to you (exit code 2) with what it did and what it saw.

  --until <check>     done when this holds; repeat for several (all must). text:<s> (default), no-text:<s>,
                      url:<s>, title:<s>, selector:<css>, js:<expression>. Without one, "done" is the model's word.
  --data <key>=<v>    a value for the goal; the key names the field ("Email=ada@x.com"); v may be op://vault/item/field
  --allow <verb>      let it click a consequential button the goal doesn't name (delete, send, pay, ...; * = any)
  --url <url>         open this page first (--profile <email>: in that Chrome profile, for a new session)
  --tab <id>          which of your tabs (default: the one you used last)
  --max-steps <n>     default 25        --timeout <seconds>  default 120
  --decider <name>    auto (default: jev, then claude), jev, claude, none
  --min-confidence <p> hand back below this (default 0.5)      --no-flow  don't replay or save flows
  --json              one JSON summary instead of lines`

// exitCode: a command that ends with a code other than 1 (do: 2 = handed back to the agent).
type exitCode struct {
	code int
	msg  string
}

func (e exitCode) Error() string { return e.msg }

type doOpts struct {
	goal            string
	checks          []map[string]string
	data            []decide.Datum
	allow           []string
	tab, maxSteps   int
	timeout         time.Duration
	decider         string
	minConf         float64
	noFlow, jsonOut bool
	url, profile    string // open this page first (in this Chrome profile, for a new session)
}

func parseDo(args []string) (doOpts, error) {
	o := doOpts{maxSteps: 25, timeout: 120 * time.Second, decider: "auto", minConf: 0.5}
	next := func(i *int, flag string) (string, error) {
		if *i+1 >= len(args) {
			return "", fmt.Errorf("%s needs a value\n\n%s", flag, doUsage)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		var v string
		var err error
		switch a {
		case "--until", "--data", "--allow", "--tab", "--max-steps", "--timeout", "--decider", "--min-confidence", "--url", "--profile":
			if v, err = next(&i, a); err != nil {
				return o, err
			}
		}
		switch a {
		case "--until":
			kind, val, ok := strings.Cut(v, ":")
			switch kind {
			case "text", "no-text", "url", "title", "selector", "js":
			default:
				kind, val, ok = "text", v, true
			}
			if !ok || val == "" {
				return o, fmt.Errorf("--until %q: say what to look for", v)
			}
			o.checks = append(o.checks, map[string]string{"kind": kind, "value": val})
		case "--data":
			k, val, ok := strings.Cut(v, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return o, fmt.Errorf("--data %q: use key=value", v)
			}
			o.data = append(o.data, decide.Datum{Key: strings.TrimSpace(k), Value: val})
		case "--allow":
			o.allow = append(o.allow, strings.ToLower(v))
		case "--url":
			o.url = v
		case "--profile":
			o.profile = v
		case "--tab":
			o.tab, err = strconv.Atoi(v)
		case "--max-steps":
			o.maxSteps, err = strconv.Atoi(v)
		case "--timeout":
			var n int
			n, err = strconv.Atoi(strings.TrimSuffix(v, "s"))
			o.timeout = time.Duration(n) * time.Second
		case "--decider":
			o.decider = v
		case "--min-confidence":
			o.minConf, err = strconv.ParseFloat(v, 64)
		case "--no-flow":
			o.noFlow = true
		case "--json":
			o.jsonOut = true
		case "-h", "--help":
			return o, errors.New(doUsage)
		default:
			if strings.HasPrefix(a, "--") {
				return o, fmt.Errorf("unknown option %s\n\n%s", a, doUsage)
			}
			if o.goal != "" {
				return o, fmt.Errorf("one goal, in quotes\n\n%s", doUsage)
			}
			o.goal = a
		}
		if err != nil {
			return o, fmt.Errorf("%s %q: %v", a, v, err)
		}
	}
	if strings.TrimSpace(o.goal) == "" {
		return o, errors.New(doUsage)
	}
	switch o.decider {
	case "auto", "jev", "claude", "none":
	default:
		return o, fmt.Errorf("--decider: auto, jev, claude or none")
	}
	return o, nil
}

// driver talks to the relay's /driver endpoints over one kept-alive connection (the relay checks who we are once).
type driver struct {
	c      *cli
	tok    string
	client *http.Client
	tab    int
}

func (d *driver) call(path string, body, out any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/driver/%s?token=%s", d.c.Port, path, d.tok), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	res, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("the relay did not answer: %s", strings.ReplaceAll(err.Error(), d.tok, "<secret>"))
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var e struct {
		Error string `json:"error"`
		Tab   int    `json:"tab"`
	}
	json.Unmarshal(raw, &e)
	if e.Error != "" {
		return errors.New(e.Error)
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("the relay answered HTTP %d", res.StatusCode)
	}
	if e.Tab != 0 {
		d.tab = e.Tab
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

type actResult struct {
	Stale     string      `json:"stale"`
	Refused   string      `json:"refused"`
	Navigated bool        `json:"navigated"`
	Dialogs   []string    `json:"dialogs"`
	Page      decide.Page `json:"page"`
	Readback  struct {
		Changed bool  `json:"changed"`
		Gone    bool  `json:"gone"`
		Value   any   `json:"value"`
		Filled  *bool `json:"filled"`
		Checked *bool `json:"checked"`
	} `json:"readback"`
}

// doSummary is what the agent gets at the end.
type doSummary struct {
	Status     string           `json:"status"` // done, done-unverified, handed-back
	Why        string           `json:"why,omitempty"`
	Steps      []decide.Step    `json:"steps"`
	Seconds    float64          `json:"seconds"`
	URL        string           `json:"url"`
	Title      string           `json:"title"`
	Candidates []decide.Element `json:"candidates,omitempty"`
	Decider    string           `json:"decider"`
	Flow       string           `json:"flow,omitempty"`
}

func (c *cli) do() error {
	o, err := parseDo(c.args[2:])
	if err != nil {
		return err
	}
	tok, err := c.token()
	if err != nil {
		return err
	}
	tr := &http.Transport{MaxIdleConnsPerHost: 1, MaxConnsPerHost: 1, IdleConnTimeout: 5 * time.Minute}
	// a fill from 1Password waits for the person's approval (Touch ID, or their iPhone): allow for it
	d := &driver{c: c, tok: tok, client: &http.Client{Transport: tr, Timeout: 6 * time.Minute}, tab: o.tab}
	models, names := c.deciders(o.decider)
	defer func() {
		for _, m := range models {
			m.Close()
		}
	}()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout+30*time.Second)
	defer cancel()

	if o.url != "" {
		u := o.url
		if !strings.Contains(u, "://") {
			u = "https://" + u
		}
		if err := d.call("open", map[string]any{"text": u, "profile": o.profile}, nil); err != nil {
			return err
		}
	}
	var page decide.Page
	if err := d.call("observe", map[string]any{"tab": d.tab}, &page); err != nil {
		return err
	}
	data := decide.Literals(o.goal, o.data)
	keys := []string{}
	for _, x := range o.data {
		keys = append(keys, x.Key)
	}
	site, shape := decide.Site(page.URL), decide.Shape(o.goal, o.data)
	flows := decide.Flows{Dir: filepath.Join(c.Home, "flows")}
	flow, haveFlow := decide.Flow{}, false
	if !o.noFlow {
		flow, haveFlow = flows.Find(site, shape)
	}
	d.call("note", map[string]any{"tab": d.tab, "event": "start", "detail": map[string]any{"goal": cut(decide.Shape(decide.Redact(o.goal, o.data), o.data), 200), "data": keys,
		"decider": strings.Join(names, ","), "flow": haveFlow}}, nil)

	sum := doSummary{Decider: strings.Join(names, ",")}
	if haveFlow {
		sum.Flow = fmt.Sprintf("%d steps, saved %s", len(flow.Steps), flow.Saved.Format("2006-01-02"))
	}
	var hist []decide.History
	used := map[string]bool{}
	flowAt, retries := 0, 0
	var lastReq *decide.Request
	say := func(format string, a ...any) {
		if !o.jsonOut {
			c.printf(format+"\n", a...)
		}
	}
	finish := func(status, why string) error {
		sum.Status, sum.Why, sum.URL, sum.Title = status, why, page.URL, page.Title
		sum.Seconds = float64(time.Since(start).Milliseconds()) / 1000
		for _, h := range hist {
			sum.Steps = append(sum.Steps, h.Step)
		}
		if status == "handed-back" {
			if lastReq != nil {
				sum.Candidates = lastReq.Candidates()
			} else {
				sum.Candidates = decide.Shortlist(page, o.goal, data, 12)
			}
		}
		if status == "done" && len(o.checks) > 0 && !o.noFlow && len(sum.Steps) > 0 {
			if err := flows.Save(site, shape, sum.Steps); err == nil {
				say("saved as a flow for %s: the same kind of goal there replays without a model", site)
			}
		}
		d.call("note", map[string]any{"tab": d.tab, "event": "end", "detail": map[string]any{"status": status, "why": why, "steps": len(sum.Steps)}}, nil)
		if o.jsonOut {
			b, _ := json.MarshalIndent(sum, "", "  ")
			c.println(string(b))
		} else {
			c.printSummary(sum)
		}
		if status == "handed-back" {
			return exitCode{2, ""}
		}
		return nil
	}
	handBack := func(why string) error { return finish("handed-back", why) }
	datum := func(k string) (decide.Datum, bool) {
		for _, x := range data {
			if x.Key == k {
				return x, true
			}
		}
		return decide.Datum{}, false
	}

	for {
		if len(o.checks) > 0 {
			var r struct {
				All    bool     `json:"all"`
				Met    []bool   `json:"met"`
				Errors []string `json:"errors"`
			}
			if err := d.call("check", map[string]any{"tab": d.tab, "checks": o.checks}, &r); err != nil {
				return handBack("checking --until failed: " + err.Error())
			}
			if len(r.Errors) > 0 {
				return handBack("an --until check fails to run: " + strings.Join(r.Errors, "; "))
			}
			if r.All {
				return finish("done", "")
			}
		}
		// the run stays on the site it started on: the agent's data isn't typed into, nor decisions made on, another
		if s := decide.Site(page.URL); baseDomain(s) != baseDomain(site) {
			return handBack(fmt.Sprintf("the page left %s for %s", site, s))
		}
		if len(hist) >= o.maxSteps {
			return handBack(fmt.Sprintf("used all %d steps (--max-steps)", o.maxSteps))
		}
		if time.Since(start) > o.timeout {
			return handBack(fmt.Sprintf("took longer than %s (--timeout)", o.timeout))
		}
		var st decide.Step
		t0 := time.Now()
		// 1. a flow that worked here before
		if haveFlow && flowAt < len(flow.Steps) {
			fs := flow.Steps[flowAt]
			x, hasDatum := datum(fs.DataKey)
			r, ok := fs.Resolve(page)
			el, _ := page.Element(r.Node)
			if ok && fs.DataKey != "" { // a step that used the agent's data uses this run's data
				switch {
				case !hasDatum:
					ok = false
				case fs.Op == "select":
					r.Option, ok = "", false
					for _, opt := range el.Options {
						if strings.EqualFold(strings.TrimSpace(opt), strings.TrimSpace(x.Value)) {
							r.Option, ok = opt, true
						}
					}
				case fs.Op == "click" && (el.Role == "checkbox" || el.Role == "switch") && (el.Checked == "true") == decide.Truthy(x.Value):
					flowAt++ // already as this run's data wants it
					used[fs.DataKey] = true
					continue
				}
			}
			if ok && fs.Op != "done" {
				st = r // flowAt moves on once the step has worked
				st.Source, st.Confidence = "flow", 1
			} else {
				haveFlow = false // the page differs from last time: decide from here
				say("flow no longer matches at step %d; deciding from here", flowAt+1)
			}
		}
		// 2. a field the agent's data names
		if st.Op == "" {
			if f, ok := decide.FormStep(page, o.data, used); ok {
				st = f
			}
		}
		// 3. a decision model
		if st.Op == "" {
			if len(models) == 0 {
				return handBack("the next step needs a decision, and no decider is set up (`" + c.Name + " decider`)")
			}
			lastReq = decide.Build(page, o.goal, data, hist, 15)
			var ans map[string]decide.Answer
			var used string
			var errs []string
			for _, m := range models {
				a, err := m.Ask(ctx, lastReq)
				if err == nil {
					ans, used = a, m.Name()
					break
				}
				errs = append(errs, err.Error())
			}
			if ans == nil {
				return handBack("the decider failed: " + strings.Join(errs, "; "))
			}
			if st, err = lastReq.Read(ans, used); err != nil {
				return handBack(used + ": " + err.Error())
			}
			switch {
			case st.Op == "done" && len(o.checks) == 0:
				return finish("done-unverified", used+" says the goal is met (no --until to check it)")
			case st.Op == "done":
				return handBack(used + " says the goal is met, but --until doesn't hold yet")
			case st.Op == "blocked":
				return handBack(used + ": nothing on the page makes progress")
			case st.Confidence < o.minConf:
				return handBack(fmt.Sprintf("%s isn't sure (%.2f) about the next step: %s", used, st.Confidence, st))
			case st.Op == "fill" && st.DataKey == "":
				return handBack(fmt.Sprintf("%q needs a value: pass --data '%s=...'", st.Label, st.Label))
			}
		}
		if verb := decide.Risky(st, o.goal, o.allow); verb != "" {
			return handBack(fmt.Sprintf("next would be %s, and the goal doesn't say to %s (pass --allow %s if it should)", st, verb, verb))
		}
		if n := len(hist); n >= 2 && !hist[n-1].Changed && !hist[n-2].Changed && hist[n-1].Step.Op == st.Op && hist[n-1].Step.Node == st.Node && st.Op != "scroll" {
			return handBack("going in circles: " + st.String() + " changed nothing")
		}
		body := map[string]any{"tab": d.tab, "kind": st.Op, "node": st.Node, "label": st.Label}
		if st.Role != "" {
			body["expect"] = map[string]string{"role": st.Role, "label": st.Label}
		}
		switch st.Op {
		case "fill":
			x, _ := datum(st.DataKey)
			body["text"] = x.Value
		case "select":
			body["option"] = st.Option
		case "key":
			body["text"] = st.Key
		case "scroll":
			body["text"] = "down"
		}
		var res actResult
		if err := d.call("act", body, &res); err != nil {
			return handBack(fmt.Sprintf("%s failed: %v", st, err))
		}
		if res.Stale != "" || res.Refused != "" {
			if res.Page.URL != "" {
				page = res.Page
			}
			retries++
			if retries > 2 {
				return handBack(fmt.Sprintf("%s: %s%s", st, res.Stale, res.Refused))
			}
			say("  %s: %s%s; looking again", st, res.Stale, res.Refused)
			continue
		}
		retries = 0
		if st.Source == "flow" {
			flowAt++
		}
		if st.Op == "fill" || st.Op == "select" || st.DataKey != "" {
			used[st.DataKey] = true
		}
		changed := res.Navigated || res.Readback.Changed || res.Readback.Gone || res.Page.Key != page.Key
		hist = append(hist, decide.History{Step: st, Changed: changed})
		page = res.Page
		conf := ""
		if st.Source == "jev" || st.Source == "claude" {
			conf = fmt.Sprintf(" %.2f", st.Confidence)
		}
		say("%2d. %s  (%s%s, %d ms)", len(hist), st, st.Source, conf, time.Since(t0).Milliseconds())
	}
}

func (c *cli) printSummary(s doSummary) {
	switch s.Status {
	case "done":
		c.printf("DONE in %d steps, %.1fs: --until holds. %s · %s\n", len(s.Steps), s.Seconds, s.Title, s.URL)
	case "done-unverified":
		c.printf("DONE (unverified) in %d steps, %.1fs: %s. %s · %s\n", len(s.Steps), s.Seconds, s.Why, s.Title, s.URL)
	default:
		c.printf("HANDED BACK after %d steps, %.1fs: %s\n", len(s.Steps), s.Seconds, s.Why)
		c.printf("Now on: %s · %s\n", s.Title, s.URL)
		if len(s.Candidates) > 0 {
			c.printf("What it was choosing between (continue yourself: chrome-relay snapshot, then click or fill):\n")
			for i, e := range s.Candidates {
				if i == 10 {
					break
				}
				v := ""
				if e.Value != "" {
					v = fmt.Sprintf(" = %q", cut(e.Value, 40))
				}
				c.printf("  %s %q%s\n", e.Role, cut(e.Label, 70), v)
			}
		}
	}
}

// --- deciders ---

const (
	keychainService = "Chrome Relay"
	jevAccount      = "TypeSafe API key"
)

// jevKey: TYPESAFE_API_KEY, else the Keychain item `chrome-relay decider jev` saved.
func jevKey() (string, string) {
	if k := strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY")); k != "" {
		return k, "TYPESAFE_API_KEY"
	}
	if !macos.Linked {
		return "", ""
	}
	b, err := macos.KeychainGet(keychainService, jevAccount)
	if err != nil {
		return "", ""
	}
	return strings.TrimSpace(string(b)), "Keychain"
}

func claudePath() string {
	p, err := exec.LookPath("claude")
	if err != nil {
		return ""
	}
	return p
}

// deciders in the order they're tried: Jev if there's a key, then Haiku through Claude Code if it's installed
// (config "decider": {"jev": "off"} or {"claude": "off"} turns either off).
func (c *cli) deciders(choice string) ([]decide.Model, []string) {
	if c.models != nil {
		return c.models(choice)
	}
	cfg := c.config().obj("decider")
	off := func(k string) bool { return cfg != nil && cfg.str(k) == "off" }
	var out []decide.Model
	var names []string
	if (choice == "auto" && !off("jev")) || choice == "jev" {
		if k, _ := jevKey(); k != "" {
			out, names = append(out, &decide.Jev{Key: k}), append(names, "jev")
		}
	}
	if (choice == "auto" && !off("claude")) || choice == "claude" {
		if p := claudePath(); p != "" {
			out, names = append(out, &decide.Claude{Path: p}), append(names, "claude")
		}
	}
	if len(names) == 0 {
		names = []string{"none"}
	}
	return out, names
}

// baseDomain: the registrable part of a host, roughly ("app.example.com" and "example.com" are one site; a
// two-letter country code takes three labels, as in "shop.example.co.uk").
func baseDomain(host string) string {
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	parts := strings.Split(host, ".")
	n := 2
	if len(parts) >= 3 && len(parts[len(parts)-1]) == 2 && len(parts[len(parts)-2]) <= 3 {
		n = 3
	}
	if len(parts) <= n {
		return host
	}
	return strings.Join(parts[len(parts)-n:], ".")
}
