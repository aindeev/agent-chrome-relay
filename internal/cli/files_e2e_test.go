package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// e2eStdin: what `chrome-relay save -` reads in these tests.
var e2eStdin io.Reader

const statementPDF = "%PDF-1.4\nfake statement for March\n%%EOF\n"

// fileSite: a bank's statements page as Wealthsimple builds it (a blank popup, a signed link from its API, then the
// popup sent there, to a PDF shown inline), and the other ways pages hand over files.
func fileSite(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/statements":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!doctype html><title>Statements</title><h1>Statements</h1>
<button id="s">March statement</button>
<a href="/files/report.csv">CSV report</a>
<button onclick="const a = document.createElement('a'); a.href = window.URL.createObjectURL(new Blob(['a,b\n1,2\n'], {type: 'text/csv'})); a.download = 'export.csv'; a.click()">Export</button>
<button onclick="window.open(window.URL.createObjectURL(new Blob(['%PDF-1.4 blob'], {type: 'application/pdf'})))">Preview</button>
<script>document.cookie = 'session=ok; path=/';
s.onclick = () => { const w = window.open(); w.document.title = 'Loading…';
  fetch('/api/sign?doc=2024-03').then((r) => r.json()).then((j) => { w.location.href = j.url; }); };</script>`)
		case "/api/sign":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"url": "/files/statement.pdf?X-Amz-Signature=secret123"}`)
		case "/files/statement.pdf":
			if r.URL.Query().Get("X-Amz-Signature") != "secret123" {
				http.Error(w, "denied", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/pdf")
			io.WriteString(w, statementPDF)
		case "/files/report.csv":
			w.Header().Set("Content-Type", "text/csv")
			w.Header().Set("Content-Disposition", `attachment; filename="report 2024.csv"`)
			fmt.Fprint(w, "x,y\n3,4\n")
		case "/private/doc.pdf":
			if c, err := r.Cookie("session"); err != nil || c.Value != "ok" {
				http.Error(w, "sign in first", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/pdf")
			fmt.Fprint(w, "%PDF-1.4 private")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDriveFiles(t *testing.T) {
	e := newE2E(t, true)
	site := fileSite(t)
	dir := t.TempDir()
	run := func(wantCode int, args ...string) string {
		t.Helper()
		start := time.Now()
		out, code := e.run(args...)
		t.Logf("%s (%.1fs)\n%s", strings.Join(args, " "), time.Since(start).Seconds(), out)
		if code != wantCode {
			t.Errorf("%s: exit %d, want %d", strings.Join(args, " "), code, wantCode)
		}
		return out
	}
	has := func(name, want string) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(b) != want {
			t.Errorf("%s: %q, %v; want %q", name, b, err, want)
		}
	}
	run(0, "open", site.URL+"/statements")

	// the popup gets a signed link, then turns into the PDF: saved, the popup gone, the page as it was
	out := run(0, "click", "March statement", "--download", "--into", dir)
	if !strings.Contains(out, "↓ saved statement.pdf") || strings.Contains(out, "secret123") {
		t.Error("the statement is saved, and the signed link's secret isn't printed:", out)
	}
	has("statement.pdf", statementPDF)
	if out := run(0, "tabs"); strings.Count(out, "\n") != 1 || !strings.Contains(out, "/statements") {
		t.Error("the popup closed once its page turned into the file:", out)
	}
	if out := run(0, "get", "text", "h1"); !strings.Contains(out, "Statements") {
		t.Error("the page is still there:", out)
	}
	if out := run(0, "errors"); !strings.HasPrefix(out, "nothing") {
		t.Error("the page threw nothing (the popup's document took its title):", out)
	}

	// a download link: the file, and the page stays where it was
	run(0, "click", "CSV report", "--download", "--into", dir)
	has("report 2024.csv", "x,y\n3,4\n")
	if out := run(0, "get", "url"); !strings.Contains(out, "/statements") {
		t.Error("a download leaves the page where it was:", out)
	}
	// a file the page's script makes, and a popup of one
	run(0, "click", "Export", "--download", "--into", dir)
	has("export.csv", "a,b\n1,2\n")
	run(0, "click", "Preview", "--download", "--into", dir)
	has("download.pdf", "%PDF-1.4 blob")
	// the same name again: never over the file that's there
	run(0, "click", "CSV report", "--download", "--into", dir)
	has("report 2024 (1).csv", "x,y\n3,4\n")

	// without --into it stays in the session's folder until moved
	out = run(0, "click", "CSV report")
	if !strings.Contains(out, "kept until this session ends") {
		t.Error("where an unmoved file is:", out)
	}
	out = run(0, "downloads", "--into", dir)
	if !strings.Contains(out, "(moved already)") || !strings.Contains(out, "report 2024 (2).csv") {
		t.Error("downloads lists them all and moves what's left:", out)
	}
	has("report 2024 (2).csv", "x,y\n3,4\n")

	// opening a file's address saves it
	out = run(0, "open", site.URL+"/files/statement.pdf?X-Amz-Signature=secret123", "--into", dir)
	if !strings.Contains(out, "↓ saved statement.pdf") {
		t.Error("open of a PDF saves it:", out)
	}
	has("statement (1).pdf", statementPDF)

	// save: with the tab's cookies; a list on stdin; a failure is said and counted
	run(0, "open", site.URL+"/statements")
	e2eStdin = strings.NewReader(site.URL + "/private/doc.pdf mine.pdf\n" + site.URL + "/nope?sig=hush\n")
	out = run(1, "save", "-", "--into", dir)
	e2eStdin = nil
	if !strings.Contains(out, "↓ saved mine.pdf") || !strings.Contains(out, "HTTP 404") || !strings.Contains(out, "1 of 2 not saved") || strings.Contains(out, "hush") {
		t.Error("save:", out)
	}
	has("mine.pdf", "%PDF-1.4 private")

	// Fetch is the relay's in these tabs
	if out := run(1, "cdp", "Fetch.disable"); !strings.Contains(out, "uses Fetch in your tabs") {
		t.Error("cdp Fetch.* refused:", out)
	}
	run(0, "close", "--all")
	for i := 0; ; i++ {
		folders, _ := filepath.Glob(filepath.Join(e.home, "downloads", "*"))
		if len(folders) == 0 {
			break
		}
		if i == 50 {
			t.Fatal("the session's files go with it:", folders)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
