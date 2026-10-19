package relay

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Files a page hands an agent (a download, a PDF, a file its script builds) are saved by the relay, into a folder
// of the session's own, instead of being left to Chrome: Chrome would show a PDF in its viewer, or put a download in
// the person's Downloads with its bubble on their screen, and the agent would get neither. Only in sessions the
// relay holds (`chrome-relay open`): agent-browser connections keep Chrome's own downloads.

const (
	maxFileSize = 2 << 30         // per file
	maxFiles    = 1000            // per session
	fileChunk   = 1 << 20         // per IO.read
	inputWindow = 1 * time.Minute // a page's file is saved only this soon after the agent's own click, key or open
)

// download: one file saved for an agent.
type download struct {
	ID    int    `json:"id"`
	Tab   int    `json:"tab"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Type  string `json:"type"`
	From  string `json:"from"`  // the address it came from (the page's, for a file its script made), without the query
	State string `json:"state"` // saving, saved, failed, blocked
	Error string `json:"error,omitempty"`
}

// pageFile: a file the page script is handing over in chunks (nil: one being ignored).
type pageFile struct {
	d *download
	f *os.File
}

// fetchPatterns: every document response in a held session's tabs pauses at its headers, for fileResponse to look at.
var fetchPatterns = map[string]any{"patterns": []map[string]any{{"urlPattern": "*", "resourceType": "Document", "requestStage": "Response"}}}

func (a *Agent) dlDir() string { return filepath.Join(a.s.opt.Home, "downloads", a.sid) }

// recentInput: the agent clicked, typed or opened a page in one of its tabs (a popup's opener included) within
// inputWindow, so a page can't fill the folder with files of its own accord.
func (a *Agent) recentInput() bool {
	for _, at := range a.lastInput {
		if time.Since(at) < inputWindow {
			return true
		}
	}
	return false
}

// mediaType: "application/pdf" from "application/pdf; charset=binary".
func mediaType(contentType string) string {
	t, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(t))
}

// pageType: what Chrome shows as a page (or as text, an image, a player) rather than downloading or opening in a
// viewer.
func pageType(t string) bool {
	switch t {
	case "text/csv", "text/calendar", "text/vcard", "text/x-vcard":
		return false
	case "application/xml", "application/json", "application/javascript", "application/x-javascript", "application/ecmascript", "multipart/x-mixed-replace":
		return true
	}
	for _, p := range []string{"text/", "image/", "video/", "audio/"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return strings.HasSuffix(t, "+xml") || strings.HasSuffix(t, "+json")
}

// fileResponse: a document response that is a file, not a page: an attachment, or (in the tab's own frame) a type
// Chrome would download or open in a viewer (a PDF). h has lower-case names.
func fileResponse(status int, h map[string]string, main bool) bool {
	if status < 200 || status > 299 || status == 204 {
		return false
	}
	if disp, _, _ := strings.Cut(h["content-disposition"], ";"); strings.EqualFold(strings.TrimSpace(disp), "attachment") {
		return true
	}
	t := mediaType(h["content-type"])
	return main && t != "" && !pageType(t)
}

var quotedName = regexp.MustCompile(`(?i)filename\s*=\s*"?([^";]+)`)

// dispositionName: the file name a Content-Disposition header gives, if any.
func dispositionName(cd string) string {
	if cd == "" {
		return ""
	}
	if _, p, err := mime.ParseMediaType(cd); err == nil { // filename*=UTF-8''... too
		return p["filename"]
	}
	if m := quotedName.FindStringSubmatch(cd); m != nil {
		return m[1]
	}
	return ""
}

var typeExt = map[string]string{"application/pdf": ".pdf", "application/zip": ".zip", "text/csv": ".csv", "application/json": ".json",
	"text/plain": ".txt", "text/html": ".html", "image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/svg+xml": ".svg",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": ".xlsx", "application/vnd.ms-excel": ".xls",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": ".docx", "text/calendar": ".ics", "application/x-ofx": ".ofx"}

// fileName: a safe name for a saved file: the one the site gave, else the address's last part, else "download",
// with an extension for its type. Never a path, a hidden file or a name that reads differently than it is.
func fileName(hint, from, typ string) string {
	n := hint
	if n == "" {
		if u, err := url.Parse(from); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
			n = path.Base(u.Path)
		}
	}
	n = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' || r == ':' || unicode.Is(unicode.Cf, r) { // Cf: right-to-left overrides
			return '_'
		}
		return r
	}, strings.ToValidUTF8(n, "_"))
	n = strings.TrimLeft(strings.TrimSpace(n), ".")
	if n == "" {
		n = "download"
	}
	if filepath.Ext(n) == "" {
		if ext, ok := typeExt[typ]; ok {
			n += ext
		} else if exts, _ := mime.ExtensionsByType(typ); len(exts) > 0 && typ != "application/octet-stream" {
			n += exts[0]
		}
	}
	if len(n) > 200 {
		ext := filepath.Ext(n)
		if len(ext) > 20 {
			ext = ""
		}
		stem := strings.TrimSuffix(n, ext)[:200-len(ext)]
		for !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
		n = stem + ext
	}
	return n
}

// newDownload: a record, and a new file for it that no other has (name, "name (1)", ...). Must hold s.mu.
func (a *Agent) newDownload(t int, name, from, typ string) (*download, *os.File, error) {
	if len(a.downloads) >= maxFiles {
		return nil, nil, fmt.Errorf("this session saved %d files already: close it and start another", maxFiles)
	}
	dir := a.dlDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	base := fileName(name, from, typ)
	ext := filepath.Ext(base)
	for i := 0; i < 1000; i++ {
		n := base
		if i > 0 {
			n = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(base, ext), i, ext)
		}
		f, err := os.OpenFile(filepath.Join(dir, n), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		a.dlSeq++
		d := &download{ID: a.dlSeq, Tab: t, Name: n, Path: f.Name(), Type: typ, From: cleanURL(from), State: "saving"}
		a.downloads = append(a.downloads, d)
		return d, f, nil
	}
	return nil, nil, errors.New("too many files with that name")
}

// blockDownload: a file the page tried to hand over that the relay refused. Must hold s.mu.
func (a *Agent) blockDownload(t int, name, from, typ, why string) {
	a.dlSeq++
	a.downloads = append(a.downloads, &download{ID: a.dlSeq, Tab: t, Name: fileName(name, from, typ), Type: typ, From: cleanURL(from), State: "blocked", Error: why})
	a.s.audit(a, "download blocked", "tab", t, "url", cleanURL(from), "why", why)
}

// finishDownload: the file is complete (err nil), or gone. Must hold s.mu.
func (a *Agent) finishDownload(d *download, f *os.File, err error) {
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(d.Path)
		d.State, d.Error, d.Path = "failed", err.Error(), ""
		a.s.audit(a, "download failed", "tab", d.Tab, "file", d.Name, "url", d.From, "why", trunc(err.Error(), 200))
		return
	}
	quarantine(d.Path)
	d.State = "saved"
	a.s.log("tab", d.Tab, "saved", d.Name, d.Size, "bytes")
	a.s.audit(a, "download", "tab", d.Tab, "file", d.Name, "size", d.Size, "type", d.Type, "url", d.From)
}

// readStream copies a DevTools stream (a response body) into f. Must hold s.mu (released while each chunk is read).
func (a *Agent) readStream(p *profile, t int, handle string, f *os.File, d *download) error {
	defer a.cdpSend(p, t, "IO.close", map[string]any{"handle": handle})
	for {
		r := a.cdp(p, t, "IO.read", map[string]any{"handle": handle, "size": fileChunk})
		if r.Error != "" {
			return fmt.Errorf("Chrome stopped handing it over: %s", r.Error)
		}
		var c struct {
			Data   string `json:"data"`
			Base64 bool   `json:"base64Encoded"`
			EOF    bool   `json:"eof"`
		}
		if err := json.Unmarshal(r.Result, &c); err != nil {
			return err
		}
		b := []byte(c.Data)
		if c.Base64 {
			var err error
			if b, err = base64.StdEncoding.DecodeString(c.Data); err != nil {
				return err
			}
		}
		if d.Size+int64(len(b)) > maxFileSize {
			return fmt.Errorf("larger than %d GB", maxFileSize>>30)
		}
		if _, err := f.Write(b); err != nil {
			return err
		}
		d.Size += int64(len(b))
		if c.EOF {
			return nil
		}
	}
}

// onResponse: a document response in a held session's tab, paused at its headers. A page goes on to Chrome at
// once; a file is saved, and its navigation cancelled, as a download leaves the page where it was. Must hold s.mu.
func (a *Agent) onResponse(t int, params json.RawMessage) {
	var e struct {
		RequestID string `json:"requestId"`
		FrameID   string `json:"frameId"`
		Request   struct {
			URL string `json:"url"`
		} `json:"request"`
		Status  int `json:"responseStatusCode"`
		Headers []struct {
			Name, Value string
		} `json:"responseHeaders"`
	}
	p := a.p()
	if json.Unmarshal(params, &e) != nil || e.RequestID == "" || p == nil {
		return
	}
	h := map[string]string{}
	for _, x := range e.Headers {
		h[strings.ToLower(x.Name)] = x.Value
	}
	main := a.mainFrames[t] == "" || e.FrameID == a.mainFrames[t] // a new popup's frame isn't known yet
	if !fileResponse(e.Status, h, main) {
		a.cdpSend(p, t, "Fetch.continueRequest", map[string]any{"requestId": e.RequestID})
		return
	}
	name, typ := dispositionName(h["content-disposition"]), mediaType(h["content-type"])
	if main { // the page stays where it was: nothing to wait for
		a.fileAt[t] = time.Now()
		delete(a.unsettled, t)
	}
	if !a.recentInput() {
		a.cdpSend(p, t, "Fetch.failRequest", map[string]any{"requestId": e.RequestID, "errorReason": "BlockedByClient"})
		a.blockDownload(t, name, e.Request.URL, typ, "the page started it on its own, not within a minute of your own click, key or open")
		return
	}
	d, f, err := a.newDownload(t, name, e.Request.URL, typ)
	if err != nil {
		a.s.log("tab", t, "can't save a file:", err.Error())
		a.cdpSend(p, t, "Fetch.failRequest", map[string]any{"requestId": e.RequestID, "errorReason": "BlockedByClient"})
		a.blockDownload(t, name, e.Request.URL, typ, err.Error())
		return
	}
	go func() { // the event loop must not wait for the body
		a.s.mu.Lock()
		defer a.s.mu.Unlock()
		a.captureResponse(p, t, e.RequestID, d, f)
	}()
}

// captureResponse saves a paused response's body into d's file, then cancels its navigation. A popup opened only
// for the file closes, as Chrome closes one whose page turned into a download. Must hold s.mu.
func (a *Agent) captureResponse(p *profile, t int, reqID string, d *download, f *os.File) {
	var err error
	r := a.cdp(p, t, "Fetch.takeResponseBodyAsStream", map[string]any{"requestId": reqID})
	var st struct {
		Stream string `json:"stream"`
	}
	if r.Error == "" && json.Unmarshal(r.Result, &st) == nil && st.Stream != "" {
		err = a.readStream(p, t, st.Stream, f, d)
	} else { // no streams in this Chrome: the whole body at once
		r = a.cdp(p, t, "Fetch.getResponseBody", map[string]any{"requestId": reqID})
		var body struct {
			Body   string `json:"body"`
			Base64 bool   `json:"base64Encoded"`
		}
		b := []byte{}
		if r.Error != "" {
			err = fmt.Errorf("Chrome did not hand it over: %s", r.Error)
		} else if err = json.Unmarshal(r.Result, &body); err == nil {
			b = []byte(body.Body)
			if body.Base64 {
				b, err = base64.StdEncoding.DecodeString(body.Body)
			}
		}
		if err == nil {
			_, err = f.Write(b)
			d.Size = int64(len(b))
		}
	}
	a.cdpSend(p, t, "Fetch.failRequest", map[string]any{"requestId": reqID, "errorReason": "Aborted"})
	a.finishDownload(d, f, err)
	if _, popup := a.openerOf[t]; popup && a.tabs[t] && (a.urls[t] == "" || strings.HasPrefix(a.urls[t], "about:")) {
		a.onDetached(t, detach{})
		a.s.extSend(p, map[string]any{"op": "close", "tabId": t})
	}
}

// onPageFile: a chunk of a file the page script read for the agent (a blob: or data: address, a same-site
// <a download>). Must hold s.mu.
func (a *Agent) onPageFile(t, id int, c map[string]any) {
	key := fmt.Sprintf("%d:%d", t, id)
	last := c["last"] == true
	pf, seen := a.pageFiles[key]
	if !seen {
		name, _ := c["name"].(string)
		typ, _ := c["type"].(string)
		name, typ = cmp.Or(trunc(name, 400), "download"), mediaType(trunc(typ, 200)) // (the page's address is not the file's name)
		if !a.recentInput() {
			a.blockDownload(t, name, a.urls[t], typ, "the page made it on its own, not within a minute of your own click, key or open")
		} else if d, f, err := a.newDownload(t, name, a.urls[t], typ); err != nil {
			a.blockDownload(t, name, a.urls[t], typ, err.Error())
		} else {
			pf = &pageFile{d, f}
		}
		a.pageFiles[key] = pf
	}
	if last {
		delete(a.pageFiles, key)
	}
	if pf == nil {
		return
	}
	var err error
	if msg, _ := c["error"].(string); msg != "" {
		err = fmt.Errorf("the page could not read it: %s", trunc(msg, 200))
	}
	var b []byte
	if err == nil {
		data, _ := c["data"].(string)
		b, err = base64.StdEncoding.DecodeString(data)
	}
	if err == nil && pf.d.Size+int64(len(b)) > maxFileSize {
		err = fmt.Errorf("larger than %d GB", maxFileSize>>30)
	}
	if err == nil {
		_, err = pf.f.Write(b)
		pf.d.Size += int64(len(b))
	}
	if err != nil || last {
		a.finishDownload(pf.d, pf.f, err)
		if !last {
			a.pageFiles[key] = nil // the rest of it is ignored
		}
	}
}

// saveURL loads an address as the tab's page would (its cookies, no cross-site checks: what opening it in the tab
// would get) and saves it as a file, whatever it is. Must hold s.mu.
func (a *Agent) saveURL(t int, u, name string) (download, error) {
	if x, err := url.Parse(u); err != nil || (x.Scheme != "http" && x.Scheme != "https") {
		return download{}, fmt.Errorf("not an http(s) address: %s", trunc(u, 100))
	}
	p := a.p()
	if p == nil {
		return download{}, errors.New("Chrome Relay extension is not connected")
	}
	frame := a.mainFrames[t]
	if frame == "" {
		var tree struct {
			FrameTree struct {
				Frame struct {
					ID string `json:"id"`
				} `json:"frame"`
			} `json:"frameTree"`
		}
		if r := a.cdp(p, t, "Page.getFrameTree", map[string]any{}); r.Error != "" || json.Unmarshal(r.Result, &tree) != nil {
			return download{}, fmt.Errorf("could not reach the page: %s", r.Error)
		}
		frame = tree.FrameTree.Frame.ID
	}
	r := a.cdp(p, t, "Network.loadNetworkResource", map[string]any{"frameId": frame, "url": u, "options": map[string]any{"disableCache": false, "includeCredentials": true}})
	if r.Error != "" {
		return download{}, fmt.Errorf("could not load it: %s", r.Error)
	}
	var res struct {
		Resource struct {
			Success        bool              `json:"success"`
			NetErrorName   string            `json:"netErrorName"`
			HTTPStatusCode int               `json:"httpStatusCode"`
			Stream         string            `json:"stream"`
			Headers        map[string]string `json:"headers"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		return download{}, err
	}
	rs := res.Resource
	if !rs.Success || rs.HTTPStatusCode < 200 || rs.HTTPStatusCode > 299 {
		if rs.Stream != "" {
			a.cdpSend(p, t, "IO.close", map[string]any{"handle": rs.Stream})
		}
		if rs.HTTPStatusCode != 0 {
			return download{}, fmt.Errorf("the site answered HTTP %d", rs.HTTPStatusCode)
		}
		return download{}, fmt.Errorf("could not load it: %s", rs.NetErrorName)
	}
	h := map[string]string{}
	for k, v := range rs.Headers {
		h[strings.ToLower(k)] = v
	}
	if name == "" {
		name = dispositionName(h["content-disposition"])
	}
	d, f, err := a.newDownload(t, name, u, mediaType(h["content-type"]))
	if err != nil {
		a.cdpSend(p, t, "IO.close", map[string]any{"handle": rs.Stream})
		return download{}, err
	}
	err = a.readStream(p, t, rs.Stream, f, d)
	a.finishDownload(d, f, err)
	return *d, err
}

// downloadsSince: this session's files after id since, once those still saving are done (at most d). appear: also
// wait, as long, for one to come. Must hold s.mu (released while waiting).
func (a *Agent) downloadsSince(since int, appear bool, d time.Duration) []download {
	deadline := time.Now().Add(d)
	for {
		out, saving := []download{}, false
		for _, x := range a.downloads {
			if x.ID > since {
				out = append(out, *x)
				saving = saving || x.State == "saving"
			}
		}
		if (!saving && (len(out) > 0 || !appear)) || a.closed || time.Now().After(deadline) {
			return out
		}
		a.s.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		a.s.mu.Lock()
	}
}
