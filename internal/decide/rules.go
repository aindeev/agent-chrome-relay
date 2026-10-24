package decide

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// FormStep fills what the agent's data names, without a model: a datum whose key matches one empty field's label,
// a dropdown whose label matches and has the value as an option, a checkbox to tick. Returns false when nothing
// matches exactly one element.
func FormStep(p Page, data []Datum, used map[string]bool) (Step, bool) {
	for _, d := range data {
		if used[d.Key] || strings.HasPrefix(d.Key, "quoted") {
			continue
		}
		var match []Element
		for _, e := range p.Elements {
			if labelMatches(e.Label, d.Key) {
				match = append(match, e)
			}
		}
		if len(match) != 1 {
			continue
		}
		e := match[0]
		switch {
		case e.can("select"):
			for _, o := range e.Options {
				if strings.EqualFold(strings.TrimSpace(o), strings.TrimSpace(d.Value)) {
					if strings.EqualFold(e.Value, o) {
						used[d.Key] = true
						break
					}
					return Step{Op: "select", Node: e.Node, Role: e.Role, Label: e.Label, Option: o, DataKey: d.Key, Source: "form", Confidence: 1}, true
				}
			}
		case e.Role == "checkbox" || e.Role == "switch":
			want := truthy(d.Value)
			if (e.Checked == "true") != want {
				return Step{Op: "click", Node: e.Node, Role: e.Role, Label: e.Label, DataKey: d.Key, Source: "form", Confidence: 1}, true
			}
			used[d.Key] = true
		case e.can("fill"):
			if e.Role == "password" && !d.Secret() {
				continue // only 1Password references go into password fields this way
			}
			if !e.Empty() && (e.Role == "password" || e.Value == d.Value) {
				used[d.Key] = true
				continue
			}
			return Step{Op: "fill", Node: e.Node, Role: e.Role, Label: e.Label, DataKey: d.Key, Source: "form", Confidence: 1}, true
		}
	}
	return Step{}, false
}

// Truthy: a checkbox datum that means "checked".
func Truthy(v string) bool { return truthy(v) }

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "yes", "true", "on", "checked", "y":
		return true
	}
	return false
}

func norm(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(regexp.MustCompile(`[^\p{L}\p{N}]+`).ReplaceAllString(strings.ReplaceAll(s, "-", ""), " "))), " ")
}

// labelMatches: "E-mail address:" matches the key "email address" or "e-mail address"; whole words, in order.
func labelMatches(label, key string) bool {
	l, k := norm(label), norm(key)
	if l == "" || k == "" {
		return false
	}
	return l == k || strings.HasPrefix(l+" ", k+" ") || strings.HasSuffix(" "+l, " "+k)
}

var destructive = regexp.MustCompile(`(?i)\b(delete|remove|destroy|erase|purge|drop|revoke|terminate|cancel|unsubscribe|deactivate|disable|pay|purchase|buy|checkout|order|transfer|send|publish|deploy|merge|sync|approve|reject|archive|reset|restart|rollback|sign out|log out|authorize|allow|grant|consent)\b`)

// Risky: a click whose label is a consequential verb the goal doesn't ask for ("Delete" when the goal says
// "open the issue"). Those go back to the agent. allow lists verbs the agent passed with --allow.
func Risky(s Step, goal string, allow []string) string {
	if s.Op != "click" {
		return ""
	}
	m := destructive.FindString(s.Label)
	if m == "" {
		return ""
	}
	verb := strings.ToLower(strings.Join(strings.Fields(m), " "))
	// the goal has to name the same verb, as whole words: "sign in" doesn't allow "Sign out", "deployment" not "Deploy"
	if regexp.MustCompile(`(?i)\b` + strings.ReplaceAll(regexp.QuoteMeta(verb), " ", `\s+`) + `\b`).MatchString(goal) {
		return ""
	}
	for _, a := range allow {
		if strings.EqualFold(a, verb) || a == "*" {
			return ""
		}
	}
	return verb
}

// Flow: the steps a verified run took on a site for a kind of goal, replayed without a model next time.
type Flow struct {
	Site  string    `json:"site"`
	Goal  string    `json:"goal"` // the goal with the agent's data replaced by {key}
	Steps []Step    `json:"steps"`
	Saved time.Time `json:"saved"`
	Runs  int       `json:"runs"`
}

// Flows are kept in HOME/flows/<site>.json: one file per site, never values (only data keys).
type Flows struct{ Dir string }

// Shape: the goal with data values replaced by their keys, so "invite ada@x.com" and "invite bob@y.com" share a flow.
// Longer values go first: a first name inside a full name must not be replaced on its own, and a value must not
// match text inside a placeholder already written.
func Shape(goal string, data []Datum) string {
	type repl struct {
		value, token string
	}
	var rs []repl
	for i, d := range data {
		if d.Value != "" {
			rs = append(rs, repl{d.Value, fmt.Sprintf("\x00%d\x00", i)})
		}
	}
	slices.SortStableFunc(rs, func(a, b repl) int { return len(b.value) - len(a.value) })
	g := goal
	for _, r := range rs {
		g = strings.ReplaceAll(g, r.value, r.token)
	}
	for i, d := range data {
		if d.Value != "" {
			g = strings.ReplaceAll(g, fmt.Sprintf("\x00%d\x00", i), "{"+d.Key+"}")
		}
	}
	return strings.Join(strings.Fields(strings.ToLower(g)), " ")
}

func Site(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return ""
	}
	return p.Host
}

func (f Flows) path(site string) string {
	safe := regexp.MustCompile(`[^a-zA-Z0-9.-]`).ReplaceAllString(site, "_")
	return filepath.Join(f.Dir, safe+".json")
}

func (f Flows) all(site string) map[string]Flow {
	m := map[string]Flow{}
	if b, err := os.ReadFile(f.path(site)); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func id(shape string) string { h := sha256.Sum256([]byte(shape)); return hex.EncodeToString(h[:6]) }

// Find a flow for this site and goal shape.
func (f Flows) Find(site, shape string) (Flow, bool) {
	if f.Dir == "" || site == "" {
		return Flow{}, false
	}
	fl, ok := f.all(site)[id(shape)]
	return fl, ok
}

// Save a verified run's steps (no values: steps carry data keys).
func (f Flows) Save(site, shape string, steps []Step) error {
	if f.Dir == "" || site == "" || len(steps) == 0 {
		return nil
	}
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return err
	}
	m := f.all(site)
	old := m[id(shape)]
	clean := make([]Step, len(steps))
	for i, s := range steps {
		clean[i] = Step{Op: s.Op, Role: s.Role, Label: s.Label, DataKey: s.DataKey, Option: s.Option, Key: s.Key}
	}
	m[id(shape)] = Flow{Site: site, Goal: shape, Steps: clean, Saved: time.Now().UTC(), Runs: old.Runs + 1}
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := fmt.Sprintf("%s.%d.tmp", f.path(site), os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path(site))
}

// Forget a site's flows ("" = all).
func (f Flows) Forget(site string) error {
	if site == "" {
		return os.RemoveAll(f.Dir)
	}
	err := os.Remove(f.path(site))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// List every saved flow.
func (f Flows) List() []Flow {
	var out []Flow
	files, _ := filepath.Glob(filepath.Join(f.Dir, "*.json"))
	for _, file := range files {
		m := map[string]Flow{}
		if b, err := os.ReadFile(file); err == nil && json.Unmarshal(b, &m) == nil {
			for _, fl := range m {
				out = append(out, fl)
			}
		}
	}
	return out
}

// Resolve a recorded step on the current page: exactly one element with the same role and label that can do it.
func (s Step) Resolve(p Page) (Step, bool) {
	switch s.Op {
	case "key", "scroll", "done":
		return s, true
	}
	var hit []Element
	for _, e := range p.Elements {
		if e.Role == s.Role && e.Label == s.Label && e.can(map[string]string{"click": "click", "fill": "fill", "select": "select"}[s.Op]) {
			hit = append(hit, e)
		}
	}
	if len(hit) != 1 {
		return s, false
	}
	s.Node = hit[0].Node
	return s, true
}
