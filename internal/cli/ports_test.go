package cli

import "testing"

func TestParseSpan(t *testing.T) {
	start, end, err := parseSpan("16000-30000")
	if err != nil || start != 16000 || end != 30000 {
		t.Errorf("= %d, %d, %v", start, end, err)
	}
	if _, _, err := parseSpan(" 16000 - 30000 "); err != nil {
		t.Errorf("spaces should be tolerated: %v", err)
	}
	for _, bad := range []string{"", "16000", "abc-def", "30000-16000", "16000-70000", "80-9000", "16000-16000"} {
		if _, _, err := parseSpan(bad); err == nil {
			t.Errorf("parseSpan(%q) = nil error; want one", bad)
		}
	}
}
