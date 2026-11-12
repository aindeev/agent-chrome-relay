package relay

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadFiles(t *testing.T) {
	dir := t.TempDir()
	pdf := filepath.Join(dir, "Statement 2026-10.PDF")
	csv := filepath.Join(dir, "activity.csv")
	os.WriteFile(pdf, []byte("%PDF-1.7 bytes"), 0o600)
	os.WriteFile(csv, []byte("date,amount\n"), 0o600)

	files, err := uploadFiles([]string{pdf, csv})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "Statement 2026-10.PDF" || files[0].Type != "application/pdf" {
		t.Fatalf("got %+v", files)
	}
	if b, _ := base64.StdEncoding.DecodeString(files[0].Data); string(b) != "%PDF-1.7 bytes" {
		t.Fatalf("bytes: %q", b)
	}
	if strings.Contains(files[1].Type, ";") || !strings.HasPrefix(files[1].Type, "text/") {
		t.Fatalf("csv type %q", files[1].Type)
	}

	if _, err := uploadFiles([]string{dir}); err == nil || !strings.Contains(err.Error(), "not a file") {
		t.Fatalf("a folder: %v", err)
	}
	if _, err := uploadFiles([]string{filepath.Join(dir, "missing.pdf")}); err == nil {
		t.Fatal("a missing file uploaded")
	}
}
