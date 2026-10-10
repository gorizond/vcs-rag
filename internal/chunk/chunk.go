// Package chunk — чанкинг исходников: границы блоков Go (go/parser) и
// фолбэк-окно строк с перекрытием (FR-008).
package chunk

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
)

// Chunk — фрагмент файла; Index — порядковый номер (≥0) в файле.
type Chunk struct {
	Index int
	Text  string
}

const (
	// ponytail: стартовые значения, уточняются по фактам.
	oversizeLines = 120 // блок длиннее — режется окном
	windowLines   = 100 // размер окна фолбэка, строк
	windowOverlap = 10  // перекрытие окон, строк
)

// Split разбивает файл на чанки. Go-файлы — по границам top-level
// func/type/method (go/parser); ошибка парсинга или отсутствие деклараций —
// эвристика блочных границ; иначе — окно строк с перекрытием.
// Пустой (или whitespace-only) файл — без чанков.
func Split(path string, content []byte) []Chunk {
	if len(bytes.TrimSpace(content)) == 0 {
		return nil
	}

	var texts []string
	if strings.HasSuffix(path, ".go") {
		texts = splitGo(path, content)
	}
	if len(texts) == 0 {
		texts = splitBlocks(content)
	}
	if len(texts) == 0 {
		texts = splitWindow(content)
	}

	var final []string
	for _, t := range texts {
		if countLines(t) > oversizeLines {
			final = append(final, splitWindowText(t)...)
			continue
		}
		final = append(final, t)
	}

	chunks := make([]Chunk, 0, len(final))
	for i, t := range final {
		chunks = append(chunks, Chunk{Index: i, Text: t})
	}
	return chunks
}

// splitGo режет файл по top-level декларациям (включая методы — они FuncDecl
// с получателем). import-блок пропускается как не несущий кода.
func splitGo(path string, content []byte) []string {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, content, parser.ParseComments)
	if err != nil {
		return nil
	}
	type block struct{ start, end int }
	var blocks []block
	for _, decl := range file.Decls {
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			continue
		}
		start := fset.Position(decl.Pos()).Offset
		end := fset.Position(decl.End()).Offset
		if start < 0 || end > len(content) || start >= end {
			continue
		}
		blocks = append(blocks, block{start: start, end: end})
	}
	if len(blocks) == 0 {
		return nil
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].start < blocks[j].start })

	texts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		texts = append(texts, string(content[b.start:b.end]))
	}
	return texts
}

// countLines считает строки, игнорируя завершающий перевод строки.
func countLines(s string) int {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
