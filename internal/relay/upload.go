package relay

import (
	"encoding/base64"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
)

// maxUploadBytes: what one upload hands a page, all files together (the bytes travel through the extension).
const maxUploadBytes = 64 << 20

type uploadFile struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Modified int64  `json:"modified"`
	Data     string `json:"data"` // base64
}

// uploadFiles reads the files an upload sets on a file field, for setFilesJS.
func uploadFiles(paths []string) ([]uploadFile, error) {
	files := make([]uploadFile, 0, len(paths))
	var total int64
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a file", filepath.Base(p))
		}
		if total += st.Size(); total > maxUploadBytes {
			return nil, fmt.Errorf("over %d MB in one upload", maxUploadBytes>>20)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		typ, _, _ := strings.Cut(mime.TypeByExtension(strings.ToLower(filepath.Ext(p))), ";")
		files = append(files, uploadFile{Name: filepath.Base(p), Type: strings.TrimSpace(typ),
			Modified: st.ModTime().UnixMilli(), Data: base64.StdEncoding.EncodeToString(b)})
	}
	return files, nil
}

// setFilesJS runs on the file field: the files become its own, then input and change fire, as when a person
// picks them. Returns "" or why not.
const setFilesJS = `function (files) {
  if (!(this instanceof HTMLInputElement) || this.type !== "file") return "not a file field";
  if (files.length > 1 && !this.multiple) return "this file field takes one file";
  const dt = new DataTransfer();
  for (const f of files) {
    const s = atob(f.data), b = new Uint8Array(s.length);
    for (let i = 0; i < s.length; i++) b[i] = s.charCodeAt(i);
    dt.items.add(new File([b], f.name, { type: f.type, lastModified: f.modified }));
  }
  this.files = dt.files;
  this.dispatchEvent(new Event("input", { bubbles: true, composed: true }));
  this.dispatchEvent(new Event("change", { bubbles: true }));
  return "";
}`
