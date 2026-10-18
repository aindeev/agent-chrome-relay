package relay

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFileResponsesAndNames(t *testing.T) {
	for _, c := range []struct {
		status   int
		ct, cd   string
		main, is bool
	}{
		{200, "application/pdf", "", true, true},                         // a statement shown inline: Chrome's viewer would take it
		{200, "application/pdf", "", false, false},                       // a PDF preview in a frame of the page stays a preview
		{200, "text/html; charset=utf-8", "", true, false},               // a page
		{200, "text/csv", "", true, true},                                // Chrome downloads CSV
		{200, "text/html", `attachment; filename="a.html"`, false, true}, // an attachment, even in a frame (the hidden-frame download)
		{200, "application/json", "", true, false},
		{200, "image/svg+xml", "", true, false},
		{302, "application/pdf", "attachment", true, false}, // a redirect goes on to its target
		{404, "application/pdf", "", true, false},
		{200, "", "", true, false}, // no type: Chrome sniffs it as a page
	} {
		h := map[string]string{"content-type": c.ct, "content-disposition": c.cd}
		if got := fileResponse(c.status, h, c.main); got != c.is {
			t.Errorf("fileResponse(%d, %q, %q, main %v) = %v", c.status, c.ct, c.cd, c.main, got)
		}
	}
	for cd, want := range map[string]string{
		`attachment; filename="report 2024.csv"`:         "report 2024.csv",
		`attachment; filename*=UTF-8''%C3%A9t%C3%A9.pdf`: "été.pdf",
		`attachment; filename=Statement March 2024.pdf`:  "Statement March 2024.pdf", // unquoted, with spaces: what servers send
		`inline`: "",
	} {
		if got := dispositionName(cd); got != want {
			t.Errorf("dispositionName(%q) = %q, want %q", cd, got, want)
		}
	}
	for _, c := range []struct{ hint, from, typ, want string }{
		{"", "https://bank.example/files/statement.pdf?X-Amz-Signature=s", "application/pdf", "statement.pdf"},
		{"", "https://bank.example/download?id=7", "application/pdf", "download.pdf"},
		{"../../.ssh/authorized_keys", "", "", "_.._.ssh_authorized_keys"},
		{".hidden", "", "text/plain", "hidden.txt"},
		{"invoice‮fdp.exe", "", "", "invoice_fdp.exe"}, // a right-to-left override that would show it as "invoiceexe.pdf"
		{"", "", "", "download"},
		{"data", "", "application/octet-stream", "data"},
		{strings.Repeat("é", 150) + ".pdf", "", "", strings.Repeat("é", 98) + ".pdf"},
	} {
		if got := fileName(c.hint, c.from, c.typ); got != c.want {
			t.Errorf("fileName(%q, %q, %q) = %q, want %q", c.hint, c.from, c.typ, got, c.want)
		}
	}
}

// A held session's pages go on to Chrome at once; a file is saved only soon after the agent's own input.
func TestFilesOnlyAfterTheAgentsInput(t *testing.T) {
	h := newHarness(t)
	chrome := h.extension("me@example.com")
	_, tab := openTab(h.agent("me@example.com"), 1)
	h.s.mu.Lock()
	var a *Agent
	for x := range h.s.agents {
		a = x
	}
	a.held = true
	paused := func(id, contentType string) {
		b, _ := json.Marshal(map[string]any{"requestId": id, "frameId": "F", "request": map[string]any{"url": "https://bank.example/files/s.pdf?sig=x"},
			"responseStatusCode": 200, "responseHeaders": []map[string]string{{"name": "Content-Type", "value": contentType}}})
		a.onResponse(tab, b)
	}
	paused("page", "text/html")
	paused("unasked", "application/pdf")
	dl := a.downloads
	h.s.mu.Unlock()
	answered := func(method, id string) map[string]any {
		return chrome.next(func(m map[string]any) bool {
			p, _ := m["params"].(map[string]any)
			return m["method"] == method && p["requestId"] == id
		})
	}
	answered("Fetch.continueRequest", "page")
	if p := answered("Fetch.failRequest", "unasked")["params"].(map[string]any); p["errorReason"] != "BlockedByClient" {
		t.Error("a file the page started on its own is refused:", p)
	}
	if len(dl) != 1 || dl[0].State != "blocked" || dl[0].From != "https://bank.example/files/s.pdf" || !strings.Contains(dl[0].Error, "on its own") {
		t.Errorf("and listed as blocked, without the query: %+v", dl)
	}
	// the agent's click a moment ago: this one is the agent's, and saved
	h.s.mu.Lock()
	a.lastInput[tab] = time.Now()
	paused("asked", "application/pdf")
	h.s.mu.Unlock()
	answered("Fetch.takeResponseBodyAsStream", "asked")
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if d := a.downloads[len(a.downloads)-1]; d.Name != "s.pdf" || !strings.HasPrefix(d.Path, a.dlDir()) {
		t.Errorf("saved into the session's folder: %+v", d)
	}
	if n := len(chrome.all(func(m map[string]any) bool { return strings.HasPrefix(fmt.Sprint(m["method"]), "Fetch.") })); n < 3 {
		t.Error("Fetch commands:", n)
	}
}
