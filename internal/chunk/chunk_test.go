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

// TestSplitGoBoundaries: Go-файл режется по границам type/func/method,
// import не становится чанком, Index последователен.
func TestSplitGoBoundaries(t *testing.T) {
	chunks := Split("demo.go", []byte(goSource))
	if len(chunks) < 3 {
		t.Fatalf("чанков %d (%v), ожидалось ≥3 (type+func+method)", len(chunks), chunks)
	}
	var foundType, foundFunc, foundMethod bool
	for _, c := range chunks {
		if strings.HasPrefix(c.Text, "import") {
			t.Errorf("import не должен быть отдельным чанком: %q", c.Text)
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
		t.Errorf("границы не найдены: type=%v func=%v method=%v", foundType, foundFunc, foundMethod)
	}
	for i, c := range chunks {
		if c.Index != i {
			t.Errorf("Index[%d] = %d, ожидался %d", i, c.Index, i)
		}
	}
}

// TestSplitGoSyntaxErrorFallback: битый Go-файл уходит в эвристику блоков.
func TestSplitGoSyntaxErrorFallback(t *testing.T) {
	broken := "package demo\n\nfunc a() {\n\t// незакрытая скобка\n\nfunc b() int {\n\treturn 1\n}\n"
	chunks := Split("broken.go", []byte(broken))
	if len(chunks) < 2 {
		t.Fatalf("чанков %d, ожидалось ≥2 (эвристика блоков)", len(chunks))
	}
	joined := ""
	for _, c := range chunks {
		joined += c.Text
	}
	if !strings.Contains(joined, "func a()") || !strings.Contains(joined, "func b()") {
		t.Errorf("блоки потеряны: %q", joined)
	}
}

// TestSplitWindowOverlap: не-Go текст без маркеров режется окном 100/10
// с перекрытием 10 строк.
func TestSplitWindowOverlap(t *testing.T) {
	var lines []string
	for i := 0; i < 250; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	chunks := Split("notes.txt", []byte(strings.Join(lines, "\n")))
	if len(chunks) != 3 {
		t.Fatalf("чанков %d, ожидалось 3 (окна 100/10)", len(chunks))
	}
	tail := strings.Split(chunks[0].Text, "\n")
	head := strings.Split(chunks[1].Text, "\n")
	overlapTail := tail[len(tail)-windowOverlap:]
	overlapHead := head[:windowOverlap]
	if strings.Join(overlapTail, "\n") != strings.Join(overlapHead, "\n") {
		t.Errorf("нет перекрытия окон: %v vs %v", overlapTail, overlapHead)
	}
}

// TestSplitOversizeAndEmpty: оверсайз-блок режется окном; пустой файл — без чанков.
func TestSplitOversizeAndEmpty(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("package demo\n\nfunc big() {\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, "\t_ = %d\n", i)
	}
	sb.WriteString("}\n")
	chunks := Split("big.go", []byte(sb.String()))
	if len(chunks) < 2 {
		t.Fatalf("чанков %d, ожидалось ≥2 (оверсайз-блок режется)", len(chunks))
	}
	for _, c := range chunks {
		if n := countLines(c.Text); n > windowLines {
			t.Errorf("чанк %d длиной %d строк > %d", c.Index, n, windowLines)
		}
	}

	if chunks := Split("empty.go", nil); len(chunks) != 0 {
		t.Errorf("пустой файл: чанков %d, ожидалось 0", len(chunks))
	}
	if chunks := Split("ws.txt", []byte("  \n\t\n")); len(chunks) != 0 {
		t.Errorf("whitespace-файл: чанков %d, ожидалось 0", len(chunks))
	}
}
