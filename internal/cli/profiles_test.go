package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// An unreadable Local State is an error, not an empty list: macOS can keep Chrome's data folder from the
// terminal running setup, and a silent empty list made every `url <email>` "unknown profile".
func TestChromeProfilesReportsWhyThereIsNoList(t *testing.T) {
	home := t.TempDir()
	if got, err := chromeProfiles(home); err == nil || got != nil {
		t.Fatalf("missing Local State: %v, %v; want an error and no list", got, err)
	}
	dir := filepath.Join(home, "Library", "Application Support", "Google", "Chrome")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := `{"profile":{"info_cache":{"Default":{"user_name":"me@example.com"},"Profile 1":{"user_name":"work@example.com"},"Guest":{}}}}`
	if err := os.WriteFile(filepath.Join(dir, "Local State"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := chromeProfiles(home)
	if err != nil || len(got) != 2 || got[0] != (chromeProfile{"me@example.com", "Default"}) || got[1] != (chromeProfile{"work@example.com", "Profile 1"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

// A connected profile is added with no directory, and adding a profile again never touches what the
// person wrote (`use`, the Google accounts) or forgets a known directory.
func TestAddProfileKeepsWhatIsThere(t *testing.T) {
	profiles := newObject()
	addProfile(profiles, "me@example.com", "")
	if e := profiles.obj("me@example.com"); e.str("chromeProfile") != "" || e.str("use") != "" {
		t.Fatalf("new entry %v", e)
	}
	profiles.obj("me@example.com").set("use", "personal")
	addProfile(profiles, "me@example.com", "Default")
	addProfile(profiles, "me@example.com", "")
	if e := profiles.obj("me@example.com"); e.str("use") != "personal" || e.str("chromeProfile") != "Default" {
		t.Fatalf("entry after re-adding %v", e)
	}
	if len(profiles.keys) != 1 {
		t.Fatalf("keys %v", profiles.keys)
	}
}
