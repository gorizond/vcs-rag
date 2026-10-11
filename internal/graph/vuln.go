package graph

import (
	"time"

	"github.com/qdrant/go-client/qdrant"
)

// VulnPoint builds one vuln point (kind=vuln, F5/US6): a security finding of
// one repository, written only when the provider's security API is available
// (the worker owns the availability gate). severity is the canonical set
// critical|high|medium|low|info, state the canonical
// detected|confirmed|dismissed|resolved. Pure: no client, no I/O.
func VulnPoint(meta Meta, id, severity, scanner, state, locationFile string) *qdrant.PointStruct {
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindVuln, KeyVuln(id)).String()),
		Payload: valueMap(map[string]any{
			"kind":          string(KindVuln),
			"severity":      severity,
			"scanner":       scanner,
			"state":         state,
			"location_file": locationFile,
			"repo":          meta.Repo,
			"group_org":     meta.GroupOrg,
			"vcs_type":      meta.VCSType,
			"branch":        meta.Branch,
			"schema_ver":    meta.SchemaVer,
			"generation":    meta.Generation,
			"indexed_at":    time.Now().UTC().Format(time.RFC3339),
		}),
	}
}
