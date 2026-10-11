package graph

import (
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// Schema-v3 security family (T014/T015, US1; data-model §new nodes, research
// R2/R3): one findings listing carries the whole sub-family — vulnerability,
// finding, occurrence, scanner, identifier and scan nodes plus the relations
// between them. The builders are pure: no client, no I/O. The deterministic
// NodeID/EdgeID input of these families is the canonical Ref* key itself (the
// schema-v3 families have no opaque Key* helper): the key is stable across
// passes, and the ID excludes generation so re-indexing rewrites the same
// point.
//
// Node kinds are the canonical v3 spellings; the sweep (gc.go) generation-
// sweeps exactly these kinds behind SweepTargets.Security.

// securityNodePayload is the shared payload of the security node builders: the
// kind discriminator, the canonical node_key, the family columns in fields,
// and the scope every graph point stamps.
func securityNodePayload(meta Meta, kind Kind, nodeKey string, fields map[string]any) map[string]*qdrant.Value {
	m := map[string]any{
		"kind":       string(kind),
		"node_key":   nodeKey,
		"repo":       meta.Repo,
		"group_org":  meta.GroupOrg,
		"vcs_type":   meta.VCSType,
		"branch":     meta.Branch,
		"schema_ver": meta.SchemaVer,
		"generation": meta.Generation,
		"indexed_at": time.Now().UTC().Format(time.RFC3339),
	}
	for k, v := range fields {
		m[k] = v
	}
	return valueMap(m)
}

// vulnerabilityKey is the canonical ref of the vulnerability a finding belongs
// to: vulnerability_id is the GitLab identity; a provider without one (GitHub
// findings) falls back to the finding id, so the derived vulnerability node,
// the occurrence's vulnerability_id link and the *_by/fixes edges all agree.
func vulnerabilityKey(meta Meta, f provider.VulnerabilityFinding) string {
	id := f.VulnerabilityID
	if id == "" {
		id = f.ID
	}
	return RefVulnerability(meta.Repo, id)
}

// VulnerabilityPoint builds the vulnerability node (kind=vulnerability, T014):
// the node is INFERRED from the findings listing (R3) — GitLab's vulnerability
// list is deprecated — and is the target of has_finding/occurrence_of and the
// *_by/fixes relations. scan_id/scanner_key/primary_identifier_key carry the
// canonical refs of the sibling nodes, so a filter on any of them reaches the
// findings of one scan/scanner/identifier directly.
func VulnerabilityPoint(meta Meta, f provider.VulnerabilityFinding) *qdrant.PointStruct {
	key := vulnerabilityKey(meta, f)
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindVulnerability, key).String()),
		Payload: securityNodePayload(meta, KindVulnerability, key, map[string]any{
			"severity":               f.Severity,
			"state":                  f.State,
			"scan_id":                RefSecurityScan(meta.Repo, f.ScanID),
			"scanner_key":            f.ScannerKey,
			"primary_identifier_key": f.PrimaryIdentifierKey,
			"location_path":          f.LocationPath,
		}),
	}
}

// SecurityScanPoint builds the security_scan node (kind=security_scan, T014):
// the scan context of a findings listing; in_pipeline links it to the
// flattened ci family by pipeline_id.
func SecurityScanPoint(meta Meta, f provider.VulnerabilityFinding) *qdrant.PointStruct {
	key := RefSecurityScan(meta.Repo, f.ScanID)
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindSecurityScan, key).String()),
		Payload: securityNodePayload(meta, KindSecurityScan, key, map[string]any{
			"scan_type":   f.ScanType,
			"status":      f.ScanStatus,
			"pipeline_id": f.ScanPipelineID,
		}),
	}
}

// FindingPoint builds the finding node (kind=finding, T014): one
// vulnerability_findings record. has_finding (from security_scan) and
// detected_by/has_identifier link it to its scan, scanner and identifiers.
func FindingPoint(meta Meta, f provider.VulnerabilityFinding) *qdrant.PointStruct {
	key := RefFinding(meta.Repo, f.UUID)
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindFinding, key).String()),
		Payload: securityNodePayload(meta, KindFinding, key, map[string]any{
			"severity":               f.Severity,
			"scan_id":                RefSecurityScan(meta.Repo, f.ScanID),
			"scanner_key":            f.ScannerKey,
			"primary_identifier_key": f.PrimaryIdentifierKey,
			"location_path":          f.LocationPath,
		}),
	}
}

// VulnerabilityOccurrencePoint builds the vulnerability_occurrence node
// (kind=vulnerability_occurrence, T014). vulnerability_id is the vulnerability
// node's canonical ref (the gc.go cascade matches it against node_key), so a
// stale vulnerability takes its occurrences with it.
func VulnerabilityOccurrencePoint(meta Meta, f provider.VulnerabilityFinding) *qdrant.PointStruct {
	key := RefVulnerabilityOccurrence(meta.Repo, f.UUID)
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindVulnerabilityOccurrence, key).String()),
		Payload: securityNodePayload(meta, KindVulnerabilityOccurrence, key, map[string]any{
			"uuid":                   f.UUID,
			"vulnerability_id":       vulnerabilityKey(meta, f),
			"scan_id":                RefSecurityScan(meta.Repo, f.ScanID),
			"scanner_key":            f.ScannerKey,
			"primary_identifier_key": f.PrimaryIdentifierKey,
			"location_path":          f.LocationPath,
			"state":                  f.State,
		}),
	}
}

// VulnerabilityIdentifierPoint builds the vulnerability_identifier node
// (kind=vulnerability_identifier, T014). Its identity is host-scoped
// (RefVulnerabilityIdentifier): GitLab identifiers carry external_type + name
// (e.g. cve + CVE-2019-…), and the name IS the external identifier, so it
// doubles as the key part the has_identifier edges resolve to.
func VulnerabilityIdentifierPoint(meta Meta, host string, ident provider.VulnerabilityIdentifier) *qdrant.PointStruct {
	key := RefVulnerabilityIdentifier(host, ident.Name)
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindVulnerabilityIdentifier, key).String()),
		Payload: securityNodePayload(meta, KindVulnerabilityIdentifier, key, map[string]any{
			"external_type": ident.ExternalType,
			"name":          ident.Name,
			"url":           ident.URL,
		}),
	}
}

// VulnerabilityScannerPoint builds the vulnerability_scanner node
// (kind=vulnerability_scanner, T014). Its identity is host-scoped
// (RefVulnerabilityScanner); name is the scanner's display name (the legacy
// Scanner column of a finding), external_id its provider id.
func VulnerabilityScannerPoint(meta Meta, host string, f provider.VulnerabilityFinding) *qdrant.PointStruct {
	key := RefVulnerabilityScanner(host, f.ScannerID)
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindVulnerabilityScanner, key).String()),
		Payload: securityNodePayload(meta, KindVulnerabilityScanner, key, map[string]any{
			"external_id": f.ScannerID,
			"name":        f.Scanner,
			"vendor":      f.ScannerVendor,
		}),
	}
}

// securityEdge builds one security relation point: deterministic ID over the
// canonical endpoint keys (generation excluded) and the shared edge payload.
func securityEdge(meta Meta, et EdgeType, fromKey, fromType, toKey, toType string) *qdrant.PointStruct {
	return &qdrant.PointStruct{
		Id: qdrant.NewID(EdgeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			et, fromKey, toKey).String()),
		Payload: orbitEdgeMap(meta, et, fromKey, fromType, toKey, toType),
	}
}

// InProjectEdge — from → project (T015): projectKey is the project node's
// canonical ref (RefProject == the repo), fromType the family of the source
// (merge_request, finding, vulnerability_occurrence, vulnerability_identifier,
// security_scan, vulnerability_scanner, vulnerability, ...).
func InProjectEdge(meta Meta, fromKey, fromType, projectKey string) *qdrant.PointStruct {
	return securityEdge(meta, EdgeInProject, fromKey, fromType, projectKey, "project")
}

// InPipelineEdge — security_scan → ci (T015): pipeline ≈ the flattened ci
// family, so ciKey is the canonical key of the pipeline's ci point (its
// payload pipeline_id carries the raw pipeline id the scan lists).
func InPipelineEdge(meta Meta, scanKey, ciKey string) *qdrant.PointStruct {
	return securityEdge(meta, EdgeInPipeline, scanKey, "security_scan", ciKey, "ci")
}

// HasFindingEdge — vulnerability → vulnerability_occurrence (T015) or
// security_scan → finding: the endpoint types are explicit because the edge
// spans two pairs.
func HasFindingEdge(meta Meta, fromKey, fromType, toKey, toType string) *qdrant.PointStruct {
	return securityEdge(meta, EdgeHasFinding, fromKey, fromType, toKey, toType)
}

// OccurrenceOfEdge — vulnerability_occurrence → vulnerability (T015).
func OccurrenceOfEdge(meta Meta, occurrenceKey, vulnerabilityKey string) *qdrant.PointStruct {
	return securityEdge(meta, EdgeOccurrenceOf, occurrenceKey, "vulnerability_occurrence", vulnerabilityKey, "vulnerability")
}

// DetectedByEdge — finding/vulnerability_occurrence → vulnerability_scanner
// (T015); fromType is the source family.
func DetectedByEdge(meta Meta, fromKey, fromType, scannerKey string) *qdrant.PointStruct {
	return securityEdge(meta, EdgeDetectedBy, fromKey, fromType, scannerKey, "vulnerability_scanner")
}

// HasIdentifierEdge — finding/vulnerability_occurrence →
// vulnerability_identifier (T015); fromType is the source family.
func HasIdentifierEdge(meta Meta, fromKey, fromType, identifierKey string) *qdrant.PointStruct {
	return securityEdge(meta, EdgeHasIdentifier, fromKey, fromType, identifierKey, "vulnerability_identifier")
}

// FixesEdge — merge_request → vulnerability (T015): the MR fixing a
// vulnerability.
func FixesEdge(meta Meta, mergeRequestKey, vulnerabilityKey string) *qdrant.PointStruct {
	return securityEdge(meta, EdgeFixes, mergeRequestKey, "merge_request", vulnerabilityKey, "vulnerability")
}

// ScansEdge — vulnerability_scanner → project (T015): projectKey is the
// project node's canonical ref (RefProject == the repo).
func ScansEdge(meta Meta, scannerKey, projectKey string) *qdrant.PointStruct {
	return securityEdge(meta, EdgeScans, scannerKey, "vulnerability_scanner", projectKey, "project")
}

// ResolvedByEdge — user → vulnerability (T015): the user who resolved it.
func ResolvedByEdge(meta Meta, userKey, vulnerabilityKey string) *qdrant.PointStruct {
	return securityEdge(meta, EdgeResolvedBy, userKey, "user", vulnerabilityKey, "vulnerability")
}

// ConfirmedByEdge — user → vulnerability (T015): the user who confirmed it.
func ConfirmedByEdge(meta Meta, userKey, vulnerabilityKey string) *qdrant.PointStruct {
	return securityEdge(meta, EdgeConfirmedBy, userKey, "user", vulnerabilityKey, "vulnerability")
}

// DismissedByEdge — user → vulnerability (T015): the user who dismissed it.
func DismissedByEdge(meta Meta, userKey, vulnerabilityKey string) *qdrant.PointStruct {
	return securityEdge(meta, EdgeDismissedBy, userKey, "user", vulnerabilityKey, "vulnerability")
}
