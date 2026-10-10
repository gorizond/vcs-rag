package chunk

import (
	"fmt"
	"strings"
	"testing"
)

const goSource = `package demo

import "fmt"

type Greeter struct {
	Name string
}

func NewGreeter(name string) *Greeter {
	return &Greeter{Name: name}
}

func (g *Greeter) Hello() string {
	return fmt.Sprintf("hello %s", g.Name)
}
`

// TestSplitGoBoundaries: a Go file is split at type/func/method boundaries,
// import does not become a chunk, Index is sequential.
func TestSplitGoBoundaries(t *testing.T) {
	chunks := Split("demo.go", []byte(goSource))
	if len(chunks) < 3 {
		t.Fatalf("got %d chunks (%v), expected ≥3 (type+func+method)", len(chunks), chunks)
	}
	var foundType, foundFunc, foundMethod bool
	for _, c := range chunks {
		if strings.HasPrefix(c.Text, "import") {
			t.Errorf("import must not be a separate chunk: %q", c.Text)
		}
		if strings.HasPrefix(c.Text, "type Greeter") {
			foundType = true
		}
		if strings.HasPrefix(c.Text, "func NewGreeter") {
			foundFunc = true
		}
		if strings.HasPrefix(c.Text, "func (g *Greeter) Hello") {
			foundMethod = true
		}
	}
	if !foundType || !foundFunc || !foundMethod {
		t.Errorf("boundaries not found: type=%v func=%v method=%v", foundType, foundFunc, foundMethod)
	}
	for i, c := range chunks {
		if c.Index != i {
			t.Errorf("Index[%d] = %d, want %d", i, c.Index, i)
		}
	}
}

// TestSplitGoSyntaxErrorFallback: a broken Go file falls back to block heuristics.
func TestSplitGoSyntaxErrorFallback(t *testing.T) {
	broken := "package demo\n\nfunc a() {\n\t// unclosed brace\n\nfunc b() int {\n\treturn 1\n}\n"
	chunks := Split("broken.go", []byte(broken))
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, expected ≥2 (block heuristics)", len(chunks))
	}
	joined := ""
	for _, c := range chunks {
		joined += c.Text
	}
	if !strings.Contains(joined, "func a()") || !strings.Contains(joined, "func b()") {
		t.Errorf("blocks lost: %q", joined)
	}
}

// TestSplitWindowOverlap: non-Go text without markers is split into 100/10
// windows with a 10-line overlap.
func TestSplitWindowOverlap(t *testing.T) {
	var lines []string
	for i := 0; i < 250; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	chunks := Split("notes.txt", []byte(strings.Join(lines, "\n")))
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, expected 3 (100/10 windows)", len(chunks))
	}
	tail := strings.Split(chunks[0].Text, "\n")
	head := strings.Split(chunks[1].Text, "\n")
	overlapTail := tail[len(tail)-windowOverlap:]
	overlapHead := head[:windowOverlap]
	if strings.Join(overlapTail, "\n") != strings.Join(overlapHead, "\n") {
		t.Errorf("no window overlap: %v vs %v", overlapTail, overlapHead)
	}
}

// TestSplitOversizeAndEmpty: an oversize block is split into windows; an empty file yields no chunks.
func TestSplitOversizeAndEmpty(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("package demo\n\nfunc big() {\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, "\t_ = %d\n", i)
	}
	sb.WriteString("}\n")
	chunks := Split("big.go", []byte(sb.String()))
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, expected ≥2 (oversize block is split)", len(chunks))
	}
	for _, c := range chunks {
		if n := countLines(c.Text); n > windowLines {
			t.Errorf("chunk %d has %d lines > %d", c.Index, n, windowLines)
		}
	}

	if chunks := Split("empty.go", nil); len(chunks) != 0 {
		t.Errorf("empty file: %d chunks, expected 0", len(chunks))
	}
	if chunks := Split("ws.txt", []byte("  \n\t\n")); len(chunks) != 0 {
		t.Errorf("whitespace file: %d chunks, expected 0", len(chunks))
	}
}
