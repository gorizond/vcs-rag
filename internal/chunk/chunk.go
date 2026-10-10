// Package chunk — source chunking: Go block boundaries (go/parser) and a
// fallback line window with overlap (FR-008).
package chunk

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
)

// Chunk is a file fragment; Index is its ordinal number (≥0) within the file.
type Chunk struct {
	Index int
	Text  string
}

const (
	// ponytail: starting values, to be refined based on facts.
	oversizeLines = 120 // longer blocks are split into windows
	windowLines   = 100 // fallback window size, lines
	windowOverlap = 10  // window overlap, lines
)

// Split splits a file into chunks. Go files — at top-level func/type/method
// boundaries (go/parser); a parse error or missing declarations — block
// boundary heuristics; otherwise — a line window with overlap.
// An empty (or whitespace-only) file yields no chunks.
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

// splitGo splits a file at top-level declarations (including methods — they are
// FuncDecl with a receiver). The import block is skipped as it carries no code.
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

// countLines counts lines, ignoring the trailing newline.
func countLines(s string) int {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
