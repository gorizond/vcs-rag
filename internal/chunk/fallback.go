package chunk

import (
	"regexp"
	"strings"
)

// blockStartRe — старт блока в языках с procedure-подобными декларациями.
var blockStartRe = regexp.MustCompile(`^(func |def |class |function |type )`)

// splitBlocks — лёгкая эвристика блочных границ: блок начинается со строки,
// матчащей blockStartRe. Возвращает nil, если границ ≤1 (тогда — окно строк).
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

// splitWindow — окно строк с перекрытием: единственный чанк, если строк мало.
func splitWindow(content []byte) []string {
	return splitWindowText(string(content))
}

// splitWindowText режет текст на окна по windowLines строк с перекрытием
// windowOverlap; последнее окно заканчивается на последней строке.
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
