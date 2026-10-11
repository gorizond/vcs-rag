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

// CoverageNamespace is the schema-version-stable namespace of the coverage
// marker (T048). Sweep intentionally skips kind=coverage, so folding
// schema_ver in (as Namespace does) would orphan the old marker after a bump —
// duplicate coverage rows per repo. A fixed dedicated namespace makes every
// pass upsert the same repo+branch-scoped point; schema_ver stays informational
// in the payload.
func CoverageNamespace() UUID { return uuid5(graphSeed, "schema:coverage") }

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

// Identity keys of the Orbit-parity node families (plan R6), mirroring the
// helpers above: opaque, NUL-separated uuid5 inputs — NOT the payload keys.
func KeyUser(host, uid string) string         { return key("user", host, uid) }
func KeyNote(id string) string                { return key("note", id) }
func KeyWorkItem(itemType, iid string) string { return key("wi", itemType, iid) }
func KeyMilestone(id string) string           { return key("ms", id) }
func KeyLabel(scope, name string) string      { return key("label", scope, name) }
func KeyRunner(host, id string) string        { return key("runner", host, id) }

// Canonical payload keys (node_key / from_key / to_key, data-model §0/§1–§5).
// Unlike the opaque identity keys above (uuid5 inputs only), these are the
// human-readable forms the graph DSL matches on; a node's node_key and the
// matching edge endpoint key are the same string by construction.
func RefFile(repo, path string) string              { return repo + "|file:" + path }
func RefPkg(repo, pkg string) string                { return repo + "|pkg:" + pkg }
func RefUser(host, uid string) string               { return "user:" + host + "#" + uid }
func RefNote(repo, id string) string                { return repo + "|note#" + id }
func RefWorkItem(repo, itemType, iid string) string { return repo + "|wi:" + itemType + "#" + iid }
func RefMilestone(repo, id string) string           { return repo + "|ms#" + id }
func RefLabel(scope, name string) string            { return "lab:" + scope + "|" + name }
func RefRunner(host, id string) string              { return "runner:" + host + "#" + id }

// Canonical payload keys of the Orbit-parity gap catalog (data-model §New
// nodes, schema v3). Same contract as above: pure, deterministic string forms
// with no generation — a node's node_key and an edge endpoint key are the same
// string by construction.
func RefGroup(fullPath string) string                           { return "group:" + fullPath }
func RefProject(repo string) string                             { return repo }
func RefBranch(repo, name string) string                        { return repo + "|branch:" + name }
func RefDirectory(repo, path string) string                     { return repo + "|dir:" + path }
func RefImportedSymbol(repo, file, name string) string          { return repo + "|imsym:" + file + "|" + name }
func RefMergeRequest(repo, iid string) string                   { return repo + "|mr:" + iid }
func RefMergeRequestDiff(repo, iid, rev string) string          { return repo + "|mr:" + iid + "|diff:" + rev }
func RefVulnerability(repo, id string) string                   { return repo + "|vuln:" + id }
func RefVulnerabilityIdentifier(host, externalID string) string { return host + "|vid:" + externalID }
func RefVulnerabilityScanner(host, id string) string            { return host + "|scanner:" + id }
func RefFinding(repo, uuid string) string                       { return repo + "|finding:" + uuid }
func RefSecurityScan(repo, id string) string                    { return repo + "|scan:" + id }
func RefVulnerabilityOccurrence(repo, uuid string) string       { return repo + "|occurrence:" + uuid }

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
