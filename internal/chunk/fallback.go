package chunk

import (
	"regexp"
	"strings"
)

// blockStartRe marks a block start in languages with procedure-like declarations.
var blockStartRe = regexp.MustCompile(`^(func |def |class |function |type )`)

// splitBlocks is a lightweight block boundary heuristic: a block starts at a line
// matching blockStartRe. Returns nil if there are ≤1 boundaries (then a line window).
func splitBlocks(content []byte) []string {
	lines := strings.Split(string(content), "\n")
	var starts []int
	for i, line := range lines {
		if blockStartRe.MatchString(line) {
			starts = append(starts, i)
		}
	}
	if len(starts) <= 1 {
		return nil
	}
	var out []string
	prev := 0
	for _, s := range starts {
		if s > prev {
			if b := strings.Join(lines[prev:s], "\n"); strings.TrimSpace(b) != "" {
				out = append(out, b)
			}
		}
		prev = s
	}
	if tail := strings.Join(lines[prev:], "\n"); strings.TrimSpace(tail) != "" {
		out = append(out, tail)
	}
	return out
}

// splitWindow is a line window with overlap: a single chunk if there are few lines.
func splitWindow(content []byte) []string {
	return splitWindowText(string(content))
}

// splitWindowText splits text into windows of windowLines lines with
// windowOverlap overlap; the last window ends at the last line.
func splitWindowText(text string) []string {
	lines := strings.Split(text, "\n")
	step := windowLines - windowOverlap
	if step < 1 {
		step = 1
	}
	var out []string
	for start := 0; start < len(lines); {
		end := min(start+windowLines, len(lines))
		out = append(out, strings.Join(lines[start:end], "\n"))
		if end == len(lines) {
			break
		}
		start += step
	}
	return out
}
