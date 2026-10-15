package vault

import "testing"

func TestMonthYear(t *testing.T) {
	for raw, want := range map[string]string{"202812": "12/2028", "122028": "12/2028", "12/2028": "12/2028", "2028-12": "12/2028",
		"12/28": "12/2028", "3/2029": "03/2029", "": "", "13/2028": "", "2028": ""} {
		m, y, ok := monthYear(raw)
		got := ""
		if ok {
			got = m + "/" + y
		}
		if got != want {
			t.Errorf("monthYear(%q) = %q, want %q", raw, got, want)
		}
	}
}
