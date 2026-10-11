package provider

import "testing"

// T030: the canonical state vocabulary — only GitLab "opened" is remapped;
// merged/closed/locked/open/draft pass through.
func TestNormalizeState(t *testing.T) {
	for raw, want := range map[string]string{
		"opened": "open", "open": "open", "merged": "merged",
		"closed": "closed", "locked": "locked", "draft": "draft",
	} {
		if got := NormalizeState(raw); got != want {
			t.Errorf("NormalizeState(%q) = %q, want %q", raw, got, want)
		}
	}
}
