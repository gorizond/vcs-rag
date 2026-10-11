package chunk

import (
	"slices"
	"strings"
	"testing"
)

const graphSource = `package demo

import (
	"fmt"
	al "strings"
	_ "net/http/pprof"
)

const (
	A = 1
	B = 2
)

var V = "x"

type Point struct{ X, Y int }

func (p *Point) Len() int { return 0 }

func New() *Point { return nil }
`

// TestExtract: kinds/names/receivers/imports/package of top-level declarations.
func TestExtract(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		content     string
		wantDefs    []string // "kind:receiver:name"
		wantImports []string
		wantPkg     string
		wantErr     bool
	}{
		{
			name:        "declarations, methods and aliased imports",
			path:        "demo.go",
			content:     graphSource,
			wantDefs:    []string{"const::A", "const::B", "var::V", "type::Point", "method:*Point:Len", "func::New"},
			wantImports: []string{"fmt", "strings", "net/http/pprof"},
			wantPkg:     "demo",
		},
		{
			name:    "package without declarations",
			path:    "bare.go",
			content: "package p\n",
		},
		{
			name:    "empty content",
			path:    "empty.go",
			content: "",
			wantErr: true,
		},
		{
			name:    "unparsable content",
			path:    "broken.go",
			content: "package p\n\nfunc {",
			wantErr: true,
		},
		{
			name:    "not a go file",
			path:    "notes.txt",
			content: "package p\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defs, imports, err := Extract(tt.path, []byte(tt.content))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Extract(%q) error = nil, want error", tt.path)
				}
				if defs != nil || imports != nil {
					t.Fatalf("Extract(%q) = (%v, %v, %v), want (nil, nil, err)", tt.path, defs, imports, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Extract(%q) error = %v", tt.path, err)
			}
			var gotDefs []string
			for _, d := range defs {
				gotDefs = append(gotDefs, d.Kind+":"+d.Receiver+":"+d.Name)
				if d.FilePath != tt.path {
					t.Errorf("FilePath = %q, want %q", d.FilePath, tt.path)
				}
				if tt.wantPkg != "" && d.Package != tt.wantPkg {
					t.Errorf("Package = %q, want %q", d.Package, tt.wantPkg)
				}
			}
			if !slices.Equal(gotDefs, tt.wantDefs) {
				t.Errorf("defs = %v, want %v", gotDefs, tt.wantDefs)
			}
			var gotImports []string
			for _, im := range imports {
				gotImports = append(gotImports, im.Path)
			}
			if !slices.Equal(gotImports, tt.wantImports) {
				t.Errorf("imports = %v, want %v", gotImports, tt.wantImports)
			}
		})
	}
}

// TestExtractSignatures: signatures are stable short headers without bodies.
func TestExtractSignatures(t *testing.T) {
	defs, _, err := Extract("demo.go", []byte(graphSource))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	sigs := map[string]string{}
	for _, d := range defs {
		sigs[d.Kind+":"+d.Receiver+":"+d.Name] = d.Signature
	}
	for key, want := range map[string]string{
		"func::New":         "func New() *Point",
		"method:*Point:Len": "func (p *Point) Len() int",
	} {
		if got := sigs[key]; got != want {
			t.Errorf("Signature[%s] = %q, want %q", key, got, want)
		}
	}
	if got := sigs["type::Point"]; !strings.HasPrefix(got, "type Point struct{") {
		t.Errorf("Signature[type::Point] = %q, want prefix %q", got, "type Point struct{")
	}
	if got := sigs["const::A"]; !strings.HasPrefix(got, "const A") {
		t.Errorf("Signature[const::A] = %q, want prefix %q", got, "const A")
	}
	for key, sig := range sigs {
		if strings.Contains(sig, "\n") || strings.Contains(sig, "return") {
			t.Errorf("Signature[%s] = %q contains body/newline", key, sig)
		}
	}
}
