package chunk

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strconv"
	"strings"
)

// GraphDefinition is a top-level Go declaration (T012).
type GraphDefinition struct {
	Name      string // declared name
	Kind      string // func | method | type | const | var
	Receiver  string // receiver type for methods, "" otherwise
	Package   string // package name of the file
	FilePath  string
	Signature string // short declaration header, no body
}

// GraphImport is an import path used by the file (unquoted).
type GraphImport struct {
	Path string
}

// Extract returns top-level definitions and imports of a Go file. Pure: no I/O,
// no dependencies outside the stdlib. A non-.go path or unparsable content
// yields nil, nil, err.
func Extract(path string, content []byte) (defs []GraphDefinition, imports []GraphImport, err error) {
	if !strings.HasSuffix(path, ".go") {
		return nil, nil, fmt.Errorf("chunk: %s: not a go file", path)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, content, parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}

	pkg := file.Name.Name
	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, "\"`")
		if u, uerr := strconv.Unquote(imp.Path.Value); uerr == nil {
			p = u
		}
		imports = append(imports, GraphImport{Path: p})
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind, recv := "func", ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
				recv = exprString(fset, d.Recv.List[0].Type)
			}
			defs = append(defs, GraphDefinition{
				Name: d.Name.Name, Kind: kind, Receiver: recv, Package: pkg,
				FilePath: path, Signature: funcSignature(fset, d),
			})
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				sig := genSignature(fset, d.Tok, spec)
				switch s := spec.(type) {
				case *ast.TypeSpec:
					defs = append(defs, GraphDefinition{
						Name: s.Name.Name, Kind: "type", Package: pkg,
						FilePath: path, Signature: sig,
					})
				case *ast.ValueSpec:
					kind := "var"
					if d.Tok == token.CONST {
						kind = "const"
					}
					for _, name := range s.Names {
						defs = append(defs, GraphDefinition{
							Name: name.Name, Kind: kind, Package: pkg,
							FilePath: path, Signature: sig,
						})
					}
				}
			}
		}
	}
	return defs, imports, nil
}

// funcSignature reconstructs a short header: "func (recv) Name(args) rets".
func funcSignature(fset *token.FileSet, d *ast.FuncDecl) string {
	var b bytes.Buffer
	b.WriteString("func ")
	if d.Recv != nil && len(d.Recv.List) == 1 && len(d.Recv.List[0].Names) > 0 {
		b.WriteString("(")
		for i, n := range d.Recv.List[0].Names {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(n.Name)
		}
		b.WriteString(" ")
		printer.Fprint(&b, fset, d.Recv.List[0].Type)
		b.WriteString(") ")
	}
	b.WriteString(d.Name.Name)
	// d.Type prints as "func(args) rets"; drop the leading keyword.
	b.WriteString(strings.TrimPrefix(exprString(fset, d.Type), "func"))
	return b.String()
}

// genSignature is the first line of "<tok> <spec>" (drops multi-line bodies).
func genSignature(fset *token.FileSet, tok token.Token, spec ast.Spec) string {
	var b bytes.Buffer
	b.WriteString(tok.String())
	b.WriteString(" ")
	printer.Fprint(&b, fset, spec)
	s := b.String()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// exprString prints a single node (used for receiver types).
func exprString(fset *token.FileSet, e ast.Expr) string {
	var b bytes.Buffer
	printer.Fprint(&b, fset, e)
	return b.String()
}
