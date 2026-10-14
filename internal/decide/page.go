// Package decide picks the next browser action toward an agent's goal, for `chrome-relay do`: from data the
// agent gave (form fields), from a flow that worked before, or by asking a fast decision model (TypeSafe's Jev,
// or Claude Haiku through the person's own Claude Code). Anything it isn't sure of goes back to the agent.
package decide

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Element is one control the relay's driver saw (internal/relay/shims/driver.js).
type Element struct {
	Node     int      `json:"node"`
	Role     string   `json:"role"`
	Label    string   `json:"label"`
	Ops      []string `json:"ops"` // click, fill, select
	Value    string   `json:"value,omitempty"`
	Filled   bool     `json:"filled,omitempty"` // password fields: whether something is in it (never the value)
	Checked  string   `json:"checked,omitempty"`
	Selected string   `json:"selected,omitempty"`
	Expanded string   `json:"expanded,omitempty"`
	Options  []string `json:"options,omitempty"`
	Onscreen bool     `json:"onscreen"`
	Frame    bool     `json:"frame,omitempty"` // inside a same-origin frame
	Required bool     `json:"required,omitempty"`
}

// Can the element take this operation (click, fill, select, upload)?
func (e Element) Can(op string) bool { return e.can(op) }

func (e Element) can(op string) bool {
	for _, o := range e.Ops {
		if o == op {
			return true
		}
	}
	return false
}

// Empty: an editable field with nothing in it.
func (e Element) Empty() bool {
	if e.Role == "password" {
		return !e.Filled
	}
	return e.can("fill") && strings.TrimSpace(e.Value) == ""
}

// Page is what the driver saw.
type Page struct {
	URL      string    `json:"url"`
	Title    string    `json:"title"`
	Text     string    `json:"text"`
	Dialog   string    `json:"dialog"`
	Key      string    `json:"key"`
	Elements []Element `json:"elements"`
	Omitted  int       `json:"omitted"`
	// frames from other sites, which the page reader cannot enter
	OtherFrames []string `json:"otherFrames,omitempty"`
	Scroll      struct {
		Y      int `json:"y"`
		Height int `json:"height"`
		View   int `json:"view"`
	} `json:"scroll"`
}

// Element by node.
func (p Page) Element(node int) (Element, bool) {
	for _, e := range p.Elements {
		if e.Node == node {
			return e, true
		}
	}
	return Element{}, false
}

// Datum is one value the agent gave for the goal (--data key=value), or a quoted literal in the goal.
type Datum struct {
	Key   string
	Value string // text, or an op:// reference the relay types from 1Password
}

// Secret: an op:// reference; its value is never shown to a model or the agent.
func (d Datum) Secret() bool { return strings.HasPrefix(strings.TrimSpace(d.Value), "op://") }

var opRef = regexp.MustCompile(`op://\S+`)

// Redact: the goal as a model or the audit trail may see it: 1Password references and the agent's secret values
// replaced by their keys.
func Redact(goal string, data []Datum) string {
	for _, d := range data {
		if d.Secret() && strings.TrimSpace(d.Value) != "" {
			goal = strings.ReplaceAll(goal, strings.TrimSpace(d.Value), "{"+d.Key+"}")
		}
	}
	return opRef.ReplaceAllString(goal, "{1Password reference}")
}

func (d Datum) preview() string {
	if d.Secret() {
		return d.Key + " (from 1Password)"
	}
	v := d.Value
	if len(v) > 60 {
		v = v[:60] + "…"
	}
	return fmt.Sprintf("%s: %q", d.Key, v)
}

var quoted = regexp.MustCompile(`"([^"]{1,200})"|“([^”]{1,200})”`)

// Literals: quoted strings in the goal become data too ("search for \"chrome devtools\"").
func Literals(goal string, data []Datum) []Datum {
	out := append([]Datum{}, data...)
	for i, m := range quoted.FindAllStringSubmatch(goal, -1) {
		v := m[1]
		if v == "" {
			v = m[2]
		}
		dup := false
		for _, d := range out {
			dup = dup || d.Value == v
		}
		if !dup {
			out = append(out, Datum{Key: fmt.Sprintf("quoted%d", i+1), Value: v})
		}
	}
	return out
}

// Step is one action, decided or done.
type Step struct {
	Op         string  `json:"op"` // click, fill, select, key, scroll, done, blocked
	Node       int     `json:"node,omitempty"`
	Role       string  `json:"role,omitempty"`
	Label      string  `json:"label,omitempty"`
	DataKey    string  `json:"dataKey,omitempty"` // fill: which datum
	Option     string  `json:"option,omitempty"`  // select: which option
	Key        string  `json:"key,omitempty"`     // key: which key
	Source     string  `json:"source,omitempty"`  // form, flow, jev, claude
	Confidence float64 `json:"confidence,omitempty"`
	Why        string  `json:"why,omitempty"`
}

func (s Step) String() string {
	switch s.Op {
	case "fill":
		return fmt.Sprintf("fill %s %q ← %s", s.Role, s.Label, s.DataKey)
	case "select":
		return fmt.Sprintf("select %q in %q", s.Option, s.Label)
	case "key":
		return "press " + s.Key
	case "click":
		return fmt.Sprintf("click %s %q", s.Role, s.Label)
	}
	return s.Op
}

var word = regexp.MustCompile(`[\p{L}\p{N}]+`)
var stop = map[string]bool{"the": true, "a": true, "an": true, "of": true, "to": true, "for": true, "and": true, "or": true, "in": true,
	"on": true, "with": true, "this": true, "that": true, "it": true, "its": true, "is": true, "be": true, "by": true, "at": true, "as": true,
	"from": true, "your": true, "my": true, "me": true, "page": true}

func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range word.FindAllString(strings.ToLower(strings.ReplaceAll(s, "-", "")), -1) {
		if !stop[w] && len(w) > 1 {
			out[w] = true
		}
	}
	return out
}

var submitLike = regexp.MustCompile(`(?i)\b(submit|search|go|next|continue|save|ok|apply|sign in|log in|send|create|confirm|done|update)\b`)

// Shortlist keeps the k elements most related to the goal and data: matching words, empty fields, submit-like
// buttons, what's on screen. A small decision has a better chance of being right, and is faster.
func Shortlist(p Page, goal string, data []Datum, k int) []Element {
	gw := words(goal)
	for _, d := range data {
		for w := range words(d.Key) {
			gw[w] = true
		}
		if !d.Secret() {
			for w := range words(d.Value) {
				gw[w] = true
			}
		}
	}
	type scored struct {
		e     Element
		score float64
		i     int
	}
	all := make([]scored, 0, len(p.Elements))
	for i, e := range p.Elements {
		lw := words(e.Label + " " + strings.Join(e.Options, " "))
		s := 0.0
		for w := range lw {
			if gw[w] {
				s += 3
			}
		}
		if e.Empty() {
			s += 2
		}
		if submitLike.MatchString(e.Label) {
			s++
		}
		if e.Onscreen {
			s += 0.5
		}
		all = append(all, scored{e, s, i})
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].score > all[b].score })
	if len(all) > k {
		all = all[:k]
	}
	sort.Slice(all, func(a, b int) bool { return all[a].i < all[b].i }) // page order reads better
	out := make([]Element, len(all))
	for i, s := range all {
		out[i] = s.e
	}
	return out
}
