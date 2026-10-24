package decide

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func formPage() Page {
	return Page{URL: "https://shop.example/order", Title: "Order", Elements: []Element{
		{Node: 1, Role: "textbox", Label: "Customer name", Ops: []string{"fill", "click"}, Onscreen: true},
		{Node: 2, Role: "textbox", Label: "E-mail address:", Ops: []string{"fill", "click"}, Onscreen: true},
		{Node: 3, Role: "password", Label: "Password", Ops: []string{"fill", "click"}, Onscreen: true},
		{Node: 4, Role: "combobox", Label: "Country", Ops: []string{"select"}, Value: "Choose…", Options: []string{"Choose…", "Canada", "France"}},
		{Node: 5, Role: "checkbox", Label: "Subscribe to news", Ops: []string{"click"}, Checked: "false"},
		{Node: 6, Role: "button", Label: "Submit order", Ops: []string{"click"}},
		{Node: 7, Role: "link", Label: "Delete my account", Ops: []string{"click"}},
	}}
}

func TestLabelMatches(t *testing.T) {
	for _, c := range []struct {
		label, key string
		want       bool
	}{{"E-mail address:", "email address", true}, {"E-mail address:", "E-mail", true}, {"Customer name", "name", true},
		{"Customer name", "Customer", true}, {"Username", "name", false}, {"Subscribe to news", "subscribe", true}, {"Country", "count", false}} {
		if got := labelMatches(c.label, c.key); got != c.want {
			t.Errorf("labelMatches(%q, %q) = %v", c.label, c.key, got)
		}
	}
}

func TestFormStep(t *testing.T) {
	p := formPage()
	used := map[string]bool{}
	data := []Datum{{"Customer name", "Ada"}, {"E-mail address", "ada@x.com"}, {"Password", "hunter2"}, {"Country", "france"}, {"Subscribe", "yes"}}
	var got []string
	for i := 0; i < 6; i++ {
		s, ok := FormStep(p, data, used)
		if !ok {
			break
		}
		got = append(got, s.String())
		used[s.DataKey] = true
	}
	want := []string{`fill textbox "Customer name" ← Customer name`, `fill textbox "E-mail address:" ← E-mail address`,
		`select "France" in "Country"`, `click checkbox "Subscribe to news"`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q", got) // a plain-text password never goes into a password field this way
	}
	if s, ok := FormStep(p, []Datum{{"Password", "op://Agents/Shop/password"}}, map[string]bool{}); !ok || s.Node != 3 {
		t.Error("a 1Password reference fills the password field:", s, ok)
	}
	p.Elements[4].Checked = "true"
	if _, ok := FormStep(p, []Datum{{"Subscribe", "yes"}}, map[string]bool{}); ok {
		t.Error("an already-checked box is left alone")
	}
}

func TestRisky(t *testing.T) {
	del := Step{Op: "click", Label: "Delete my account"}
	if Risky(del, "open my account settings", nil) != "delete" {
		t.Error("an unrequested delete")
	}
	if Risky(del, "delete my account", nil) != "" || Risky(del, "open settings", []string{"delete"}) != "" || Risky(del, "x", []string{"*"}) != "" {
		t.Error("a requested or allowed delete")
	}
	if Risky(Step{Op: "click", Label: "Submit order"}, "order a pizza", nil) != "" {
		t.Error("the goal names it")
	}
	if Risky(Step{Op: "fill", Label: "Delete reason"}, "x", nil) != "" {
		t.Error("only clicks")
	}
	if Risky(Step{Op: "click", Label: "Sign out"}, "sign in to GitHub", nil) != "sign out" {
		t.Error(`"sign in" doesn't allow "Sign out"`)
	}
	if Risky(Step{Op: "click", Label: "Deploy"}, "open the deployment page", nil) != "deploy" {
		t.Error("whole words only")
	}
	if Risky(Step{Op: "click", Label: "Authorize app"}, "open the settings", nil) != "authorize" {
		t.Error("consent screens come back")
	}
}

func TestRedactAndChildEnv(t *testing.T) {
	data := []Datum{{"Password", " op://v/i/password"}, {"Email", "ada@x.com"}}
	if got := Redact("log in with op://v/i/password and op://v/other/x as ada@x.com", data); got != "log in with {Password} and {1Password reference} as ada@x.com" {
		t.Error(got)
	}
	if !data[0].Secret() {
		t.Error("a reference with a leading space is still a secret")
	}
	env := childEnv([]string{"PATH=/bin", "CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=s", "CLAUDE_CODE_MESSAGING_SOCKET=/x", "CLAUDE_CODE_OAUTH_TOKEN=keep", "HOME=/h"})
	if strings.Join(env, " ") != "PATH=/bin CLAUDE_CODE_OAUTH_TOKEN=keep HOME=/h" {
		t.Error(env)
	}
}

func TestBuildAndRead(t *testing.T) {
	data := Literals(`find "gödel numbering"`, []Datum{{"Customer name", "Ada"}, {"Password", "op://v/i/password"}})
	if len(data) != 3 || data[2].Key != "quoted1" || data[2].Value != "gödel numbering" {
		t.Fatalf("literals: %+v", data)
	}
	r := Build(formPage(), "order for Ada", data, nil, 15)
	b := string(r.JSON())
	if strings.Contains(b, "op://") {
		t.Error("a 1Password reference is never shown to a model")
	}
	q := r.Body["questions"].(map[string]any)
	for _, want := range []string{"operation", "click_target", "type_text_target", "type_text_value", "select_target"} {
		if q[want] == nil {
			t.Error("missing question", want)
		}
	}
	idx := func(label string) string {
		for i, e := range r.Candidates() {
			if e.Label == label {
				return string(rune('1' + i))
			}
		}
		t.Fatal("no", label)
		return ""
	}
	s, err := r.Read(map[string]Answer{"operation": {Choice: "TYPE_TEXT", Probabilities: map[string]float64{"TYPE_TEXT": 0.9}},
		"type_text_target": {Choice: idx("Customer name"), Probabilities: map[string]float64{idx("Customer name"): 0.8}},
		"type_text_value":  {Choice: "Customer name", Confidence: 1}}, "jev")
	if err != nil || s.Op != "fill" || s.Node != 1 || s.DataKey != "Customer name" || s.Confidence < 0.71 || s.Confidence > 0.73 {
		t.Errorf("fill: %+v %v", s, err)
	}
	s, err = r.Read(map[string]Answer{"operation": {Choice: "SELECT"}, "select_target": {Choice: idx("Country") + ":3"}}, "claude")
	if err != nil || s.Op != "select" || s.Option != "France" {
		t.Errorf("select: %+v %v", s, err)
	}
	if _, err := r.Read(map[string]Answer{"operation": {Choice: "CLICK"}, "click_target": {Choice: "99"}}, "jev"); err == nil {
		t.Error("an element that wasn't offered")
	}
}

func TestShortlistKeepsRelevant(t *testing.T) {
	p := formPage()
	for i := 0; i < 50; i++ {
		p.Elements = append(p.Elements, Element{Node: 100 + i, Role: "link", Label: "Footer link", Ops: []string{"click"}})
	}
	got := Shortlist(p, "submit the order for the customer", []Datum{{"Customer name", "Ada"}}, 5)
	labels := []string{}
	for _, e := range got {
		labels = append(labels, e.Label)
	}
	j := strings.Join(labels, "|")
	if !strings.Contains(j, "Submit order") || !strings.Contains(j, "Customer name") || strings.Contains(j, "Footer") {
		t.Errorf("shortlist: %s", j)
	}
}

func TestFlows(t *testing.T) {
	f := Flows{Dir: t.TempDir()}
	data := []Datum{{"name", "Ada"}}
	shape := Shape("Invite Ada to the team", data)
	if shape != "invite {name} to the team" {
		t.Fatal(shape)
	}
	// a first name inside a full name must not eat the longer value, or the next invite saves a different shape
	overlap := []Datum{{"first", "Ada"}, {"full", "Ada Lovelace"}}
	if got := Shape("Invite Ada Lovelace to the team", overlap); got != "invite {full} to the team" {
		t.Fatalf("shorter value wins: %q", got)
	}
	if got := Shape("Invite Bob Smith to the team", []Datum{{"first", "Bob"}, {"full", "Bob Smith"}}); got != "invite {full} to the team" {
		t.Fatalf("next run does not share the flow: %q", got)
	}
	steps := []Step{{Op: "fill", Node: 9, Role: "textbox", Label: "Name", DataKey: "name", Source: "jev", Confidence: 0.9}, {Op: "click", Role: "button", Label: "Invite"}}
	if err := f.Save("app.example", shape, steps); err != nil {
		t.Fatal(err)
	}
	fl, ok := f.Find("app.example", Shape("invite Bob to the team", []Datum{{"name", "Bob"}}))
	if !ok || len(fl.Steps) != 2 || fl.Steps[0].Node != 0 || fl.Steps[0].Source != "" || fl.Runs != 1 {
		t.Fatalf("find: %+v %v", fl, ok)
	}
	p := Page{Elements: []Element{{Node: 3, Role: "textbox", Label: "Name", Ops: []string{"fill"}}, {Node: 4, Role: "button", Label: "Invite", Ops: []string{"click"}}}}
	if s, ok := fl.Steps[1].Resolve(p); !ok || s.Node != 4 {
		t.Error("resolve", s, ok)
	}
	p.Elements = append(p.Elements, Element{Node: 5, Role: "button", Label: "Invite", Ops: []string{"click"}})
	if _, ok := fl.Steps[1].Resolve(p); ok {
		t.Error("two matches: don't guess")
	}
	if len(f.List()) != 1 || f.Forget("app.example") != nil || len(f.List()) != 0 {
		t.Error("list/forget")
	}
}

func TestJevClient(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"model":"jev-1.13.0","answers":{"operation":{"type":"choice","choice":"DONE","confidence":0.97,"probabilities":{"DONE":0.97}}}}`))
	}))
	defer srv.Close()
	r := Build(formPage(), "x", nil, nil, 5)
	ans, err := (&Jev{Key: "k", URL: srv.URL}).Ask(context.Background(), r)
	if err != nil || ans["operation"].Choice != "DONE" || got["model"] != "jev-latest" {
		t.Errorf("%v %v %v", ans, err, got["model"])
	}
	if _, err := (&Jev{Key: "bad", URL: srv.URL}).Ask(context.Background(), r); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Error("a refused key:", err)
	}
}
