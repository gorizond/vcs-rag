package worker

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// T091: the pass summary reports resumed/skipped next to the existing counters.
func TestSummaryResumedSkipped(t *testing.T) {
	sum := newSummary("index")
	sum.countRepo(nil)
	sum.countRepo(errors.New("boom"))
	sum.addIndexed(3)
	sum.addResumed(5)
	sum.addSkipped(2)
	sum.addPruned(1)

	var buf bytes.Buffer
	sum.log(slog.New(slog.NewTextHandler(&buf, nil)))
	out := buf.String()
	for _, want := range []string{
		"pass summary: index", "repos=2", "ok=1", "errors=1",
		"indexed_commits=3", "resumed=5", "skipped=2", "pruned=1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary %q does not contain %q", out, want)
		}
	}
}
