package graph

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/qdrant/go-client/qdrant"
)

// PathStatus is the honest answer of a shortest-path search
// (contracts/mcp-tools.md): a truncated or coverage-gapped search must report
// inconclusive instead of claiming the path does not exist.
type PathStatus string

const (
	PathFound        PathStatus = "found"
	PathNotFound     PathStatus = "not_found"
	PathInconclusive PathStatus = "inconclusive"
)

// PathEdge is one hop of a found path, in stored edge direction.
type PathEdge struct {
	FromKey  string
	ToKey    string
	EdgeType string
}

// PathResult is the shortest-path answer. Nodes are the canonical keys from
// FromKey to ToKey (found only); Edges one per hop. Truncated reports that a
// frontier/edge cap cut the search (coverage gaps are visible on Coverage).
type PathResult struct {
	Status    PathStatus
	Nodes     []string
	Edges     []PathEdge
	Truncated bool
	Coverage  Coverage
}

// ShortestPath finds the shortest undirected path between two canonical keys
// (T013, plan R3) with a bidirectional BFS over rel_types edges:
//
//   - the search follows stored edge direction: the forward side expands
//     from_key=frontier, the backward side to_key=frontier; endpoints bounded
//     by canonical keys — legacy-only pre-parity points join through the same
//     OR fallback as traversal/neighbours (T053) — depth by the compiled hops
//     (≤3), rel_types required;
//   - the smaller frontier is expanded each step and the intersection of the
//     two visited sets is checked after every expansion — the first meeting
//     yields the minimum number of hops;
//   - the effective depth is the compiled depth clamped to the configured
//     MCP_GRAPH_PATH_MAX_DEPTH (FR-026; the contract bound 3 stays the hard
//     ceiling), so the operator setting really bounds the search;
//   - a frontier/edge cap, or a Truncated coverage for the repo, makes a
//     missing path inconclusive, never not_found.
//
// Compile validates the query first (selectivity + rel_types); MatchNone
// short-circuits to not_found without touching the Scroller.
func ShortestPath(ctx context.Context, sc Scroller, q Query, coverage Coverage) (PathResult, error) {
	res := PathResult{Status: PathNotFound, Nodes: []string{}, Edges: []PathEdge{}, Coverage: coverage}
	if q.FromKey == "" || q.ToKey == "" {
		return PathResult{}, fmt.Errorf("graph: path_finding requires both from and to keys")
	}
	if len(q.RelTypes) == 0 {
		return PathResult{}, ErrRelTypesRequired
	}
	for _, rt := range q.RelTypes {
		if !knownEdgeType(EdgeType(rt)) {
			return PathResult{}, fmt.Errorf("graph: path_finding: invalid rel_type %q", rt)
		}
	}
	c, err := Compile(q)
	if err != nil {
		return PathResult{}, err
	}
	if c.MatchNone || sc == nil {
		return res, nil
	}
	// FR-026: the effective search depth is the compiled depth clamped to the
	// configured MCP_GRAPH_PATH_MAX_DEPTH (itself ≤ the contract bound), not
	// the fixed contract maximum.
	maxDepth := c.Hops
	if maxDepth > queryCaps.pathMaxDepth {
		maxDepth = queryCaps.pathMaxDepth
	}
	if q.FromKey == q.ToKey {
		res.Status, res.Nodes = PathFound, []string{q.FromKey}
		return res, nil
	}

	fwd := map[string]int{q.FromKey: 0}
	bwd := map[string]int{q.ToKey: 0}
	parentF := map[string]string{}
	kindF := map[string]string{}
	parentB := map[string]string{}
	kindB := map[string]string{}
	frontF, frontB := []string{q.FromKey}, []string{q.ToKey}

	truncated := false
	meet, best := "", 0

	// expand grows one side by exactly one hop level and checks the new keys
	// against the other side's visited set, keeping the minimum total depth.
	expand := func(dir Direction, fr []string, self, other map[string]int, parents, kinds map[string]string) ([]string, bool, error) {
		cut := false
		if len(fr) > maxFrontier {
			fr, cut = fr[:maxFrontier], true
		}
		leg := pathFrontierLegacy(fr)
		edges, err := sc.ScrollAll(ctx, pathHopFilter(c.Edge, dir, fr, leg, q.RelTypes), graphPageSize)
		if err != nil {
			return nil, cut, fmt.Errorf("graph: path hop (%s): %w", dir, err)
		}
		if len(edges) > maxEdgesPerHop {
			edges, cut = edges[:maxEdgesPerHop], true
		}
		var next []string
		for _, e := range edges {
			m := e.GetPayload()
			from, to := payloadString(m, "from_key"), payloadString(m, "to_key")
			parent, peer := from, to
			if dir == Incoming {
				parent, peer = to, from
			}
			// T053: a pre-parity point carries no from_key/to_key; resolve its
			// legacy endpoint spellings back into the canonical key space of
			// the visited sets (see pathLegacy).
			if parent == "" {
				parent = leg.nearKey(m, dir)
			}
			if peer == "" {
				peer = pathPeerKey(m, dir)
			}
			d, ok := self[parent]
			if !ok || peer == "" || d+1 > maxDepth {
				continue
			}
			if _, seen := self[peer]; seen {
				continue
			}
			self[peer] = d + 1
			parents[peer] = parent
			kinds[peer] = payloadString(m, "edge_type")
			next = append(next, peer)
		}
		for _, k := range next {
			if od, ok := other[k]; ok {
				if total := self[k] + od; meet == "" || total < best {
					meet, best = k, total
				}
			}
		}
		return next, cut, nil
	}

	for df, db := 0, 0; df+db < maxDepth && len(frontF) > 0 && len(frontB) > 0 && meet == ""; {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		var (
			next []string
			cut  bool
			err  error
		)
		if len(frontF) <= len(frontB) {
			next, cut, err = expand(Outgoing, frontF, fwd, bwd, parentF, kindF)
			df++
			frontF = next
		} else {
			next, cut, err = expand(Incoming, frontB, bwd, fwd, parentB, kindB)
			db++
			frontB = next
		}
		if err != nil {
			return res, err
		}
		truncated = truncated || cut
	}

	res.Truncated = truncated
	if meet == "" {
		if truncated || coverage.Truncated {
			res.Status = PathInconclusive
		}
		return res, nil
	}

	// Reconstruct: forward chain FromKey..meet, then backward chain meet..ToKey.
	fchain := []string{meet}
	for k := meet; k != q.FromKey; {
		p, ok := parentF[k]
		if !ok {
			return res, fmt.Errorf("graph: path reconstruction lost forward link at %q", k)
		}
		fchain = append(fchain, p)
		k = p
	}
	reverseStrings(fchain)
	nodes := append([]string{}, fchain...)
	edges := make([]PathEdge, 0, maxDepth)
	for i := 1; i < len(fchain); i++ {
		edges = append(edges, PathEdge{FromKey: fchain[i-1], ToKey: fchain[i], EdgeType: kindF[fchain[i]]})
	}
	for k := meet; k != q.ToKey; {
		n, ok := parentB[k]
		if !ok {
			return res, fmt.Errorf("graph: path reconstruction lost backward link at %q", k)
		}
		edges = append(edges, PathEdge{FromKey: k, ToKey: n, EdgeType: kindB[k]})
		nodes = append(nodes, n)
		k = n
	}
	res.Status, res.Nodes, res.Edges = PathFound, nodes, edges
	return res, nil
}

// pathHopFilter narrows the compiled edge filter to one hop of a path search:
// the rel_types list plus the endpoint field the direction traverses. Like the
// traversal hop filter (T049) and the single-centre neighbours filter (T051),
// the endpoint is an OR of the canonical frontier keys and the legacy
// spellings they derive (T053), so edges written before from_key/to_key existed
// stay traversable. Unlike a single-centre lookup the frontier is a set, so
// every frontier key's spelling feeds the same OR branches.
func pathHopFilter(base *qdrant.Filter, dir Direction, fr []string, leg pathLegacy, rel []string) *qdrant.Filter {
	f := &qdrant.Filter{}
	f.Must = append(f.Must, base.GetMust()...)
	if len(rel) > 0 {
		f.Must = append(f.Must, qdrant.NewMatchKeywords("edge_type", rel...))
	}
	var cond *qdrant.Condition
	if dir == Incoming {
		cond = endpointOr(
			endpointKeys{"to_key", fr},
			endpointKeys{"to_package", leg.pkgKeys},
			endpointKeys{"to_package_name", leg.pkgNames},
			endpointKeys{"to_symbol", leg.syms},
		)
	} else {
		cond = endpointOr(
			endpointKeys{"from_key", fr},
			endpointKeys{"from_file", leg.files},
		)
	}
	if cond != nil {
		f.Must = append(f.Must, cond)
	}
	return f
}

// pathLegacy is the legacy-endpoint projection of one BFS frontier (T053):
// legacyCentre derives every pre-parity spelling of the frontier's canonical
// keys, the hop filter ORs them in (symmetric to the traversal/neighbours
// fallback), and the two spellings that carry no path back to a canonical ref
// — a bare package name and a bare symbol — index the frontier key that
// produced them, so a matched legacy edge's near endpoint resolves back into
// the visited set.
type pathLegacy struct {
	files, pkgKeys, pkgNames, syms []string
	byPkgName, bySym               map[string][]string
}

// pathFrontierLegacy projects a frontier onto its legacy spellings. The
// spelling lists are deduplicated per family and sorted, so the hop filter is
// deterministic regardless of frontier order.
func pathFrontierLegacy(fr []string) pathLegacy {
	leg := pathLegacy{byPkgName: map[string][]string{}, bySym: map[string][]string{}}
	seen := map[string]bool{}
	add := func(dst *[]string, family, v string) {
		if v == "" || seen[family+"\x00"+v] {
			return
		}
		seen[family+"\x00"+v] = true
		*dst = append(*dst, v)
	}
	for _, k := range fr {
		lc := legacyCentre(k)
		add(&leg.files, "file", lc.file)
		add(&leg.pkgKeys, "pkgkey", lc.pkgKey)
		add(&leg.pkgNames, "pkgname", lc.pkgName)
		add(&leg.syms, "sym", lc.sym)
		if lc.pkgName != "" {
			leg.byPkgName[lc.pkgName] = append(leg.byPkgName[lc.pkgName], k)
		}
		if lc.sym != "" {
			leg.bySym[lc.sym] = append(leg.bySym[lc.sym], k)
		}
	}
	sort.Strings(leg.files)
	sort.Strings(leg.pkgKeys)
	sort.Strings(leg.pkgNames)
	sort.Strings(leg.syms)
	return leg
}

// nearKey resolves the near endpoint — the one already in the visited set — of
// a pre-parity edge back to its canonical key. from_file is repo-scoped by the
// edge payload and to_package carries the lossless PkgKey, so both decode
// exactly; to_package_name and to_symbol carry no full path, so the frontier
// index supplies the key that derived the spelling. "" leaves the edge
// untraversed.
func (leg pathLegacy) nearKey(m map[string]*qdrant.Value, dir Direction) string {
	repo := payloadString(m, "repo")
	if dir == Outgoing {
		if f := payloadString(m, "from_file"); f != "" {
			if repo == "" {
				return f
			}
			return RefFile(repo, f)
		}
		return ""
	}
	if v := payloadString(m, "to_package"); v != "" {
		if k := decodePkgKey(v); k != "" {
			return k
		}
		return v
	}
	if n := payloadString(m, "to_package_name"); n != "" {
		return resolveSpelling(leg.byPkgName, n, repo)
	}
	if s := payloadString(m, "to_symbol"); s != "" {
		return resolveSpelling(leg.bySym, s, repo)
	}
	return ""
}

// pathPeerKey resolves the far endpoint of an edge: the canonical key when
// present, else its legacy spelling. from_file and to_package decode back to
// their canonical refs (payload repo included); to_package_name and to_symbol
// stay raw terminal pseudo-keys, exactly as Neighbors reports them.
func pathPeerKey(m map[string]*qdrant.Value, dir Direction) string {
	repo := payloadString(m, "repo")
	if dir == Incoming {
		if f := payloadString(m, "from_file"); f != "" {
			if repo == "" {
				return f
			}
			return RefFile(repo, f)
		}
		return ""
	}
	if v := payloadString(m, "to_package"); v != "" {
		if k := decodePkgKey(v); k != "" {
			return k
		}
		return v
	}
	if n := payloadString(m, "to_package_name"); n != "" {
		return n
	}
	return payloadString(m, "to_symbol")
}

// decodePkgKey decodes the repo-namespaced pre-parity to_package spelling
// (PkgKey, keySep-joined) back to the canonical package ref it encodes — the
// encoding is lossless, unlike the bare to_package_name. "" when the spelling
// is not a PkgKey (no writer ever produced another shape; guarded anyway).
func decodePkgKey(v string) string {
	parts := strings.Split(v, keySep)
	if len(parts) != 3 || parts[0] != "pkg" {
		return ""
	}
	return RefPkg(parts[1], parts[2])
}

// resolveSpelling maps a bare legacy spelling back to the frontier key that
// produced it; several derivations are disambiguated by the edge's repo, and a
// remaining ambiguity resolves to "" — the edge is left untraversed rather
// than attributed to the wrong repo.
func resolveSpelling(idx map[string][]string, spelling, repo string) string {
	found := ""
	for _, k := range idx[spelling] {
		if repo != "" && !strings.HasPrefix(k, repo+"|") {
			continue
		}
		if found != "" {
			return ""
		}
		found = k
	}
	return found
}

// reverseStrings reverses s in place.
func reverseStrings(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
