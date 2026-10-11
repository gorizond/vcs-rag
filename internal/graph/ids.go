package graph

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
)

// UUID is an RFC 4122 v5 (SHA-1) UUID computed with the standard library,
// mirroring the pattern in internal/store/ids.go.
type UUID [16]byte

// graphSeed is the fixed root seed. Must not change: it would change every
// graph ID and break idempotent re-indexing.
var graphSeed = func() UUID {
	sum := sha1.Sum([]byte("vcs-rag-graph"))
	var u UUID
	copy(u[:], sum[:16])
	return u
}()

// Namespace derives the graph uuid5 namespace from the fixed seed folded with
// the schema version, so a schema bump yields a fresh ID space.
func Namespace(schemaVer string) UUID {
	return uuid5(graphSeed, "schema:"+schemaVer)
}

// String renders the UUID in canonical 8-4-4-4-12 form.
func (u UUID) String() string {
	hx := hex.EncodeToString(u[:])
	return hx[0:8] + "-" + hx[8:12] + "-" + hx[12:16] + "-" + hx[16:20] + "-" + hx[20:32]
}

func uuid5(ns UUID, name string) UUID {
	h := sha1.New()
	h.Write(ns[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)
	var u UUID
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return u
}

// keySep separates canonical key parts. NUL cannot occur in paths, symbols,
// or IDs, so distinct inputs never collide.
const keySep = "\x00"

// Canonical keys identify graph entities deterministically.
func KeyFile(p string) string                 { return key("file", p) }
func KeyPkg(p string) string                  { return key("pkg", p) }
func KeyDef(pkg, name string) string          { return key("def", pkg, name) }
func KeyImport(fromFile, toPkg string) string { return key("import", fromFile, toPkg) }
func KeyDefine(file, symbol string) string    { return key("define", file, symbol) }
func KeyMrFile(mrID, file string) string      { return key("mrfile", mrID, file) }
func KeyCi(level, pipelineID, jobID string) string {
	return key("ci", level, pipelineID, jobID)
}
func KeyVuln(id string) string { return key("vuln", id) }

// KeyCoverage is the canonical key of the per-repo coverage marker (T057).
func KeyCoverage(repo string) string { return key("coverage", repo) }

// PkgKey is the repo-scoped package key for edge endpoints: packages like
// "utils" or "main" collide across repositories without it.
func PkgKey(repo, pkg string) string { return key("pkg", repo, pkg) }

func key(prefix string, parts ...string) string {
	return prefix + keySep + strings.Join(parts, keySep)
}

// NodeID is the deterministic uuid5 of a node's identity.
func NodeID(ns UUID, repo, branch string, k Kind, canonicalKey string) UUID {
	return uuid5(ns, key("node", repo, branch, string(k), canonicalKey))
}

// EdgeID is the deterministic uuid5 of an edge's identity. Generation is
// deliberately excluded so re-indexing the same revision keeps IDs stable.
func EdgeID(ns UUID, repo, branch string, et EdgeType, fromKey, toKey string) UUID {
	return uuid5(ns, key("edge", repo, branch, string(et), fromKey, toKey))
}
