package graph_test

import (
	"testing"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/graph"
	"github.com/gorizond/vcs-rag/internal/provider"
)

// pointField reads one string payload field of a built graph point.
func pointField(p *qdrant.PointStruct, key string) string {
	return p.GetPayload()[key].GetStringValue()
}

// securityFixture is one GitLab-style finding carrying the whole schema-v3
// security context: scanner, scan and primary identifier.
func securityFixture() provider.VulnerabilityFinding {
	return provider.VulnerabilityFinding{
		ID: "uuid-1", UUID: "uuid-1", Severity: "high", State: "detected",
		VulnerabilityID: "42", ScanID: "9",
		ScannerID: "5", ScannerKey: "gemnasium", ScannerVendor: "GitLab",
		PrimaryIdentifierKey: "CVE-2024-0001", LocationPath: "main.go",
		ScanType: "sast", ScanStatus: "success", ScanPipelineID: "77",
		Identifiers: []provider.VulnerabilityIdentifier{
			{ExternalType: "cve", Name: "CVE-2024-0001", URL: "https://example.test/CVE-2024-0001"},
		},
	}
}

// TestSecurityFamilyShape — T021/US1: one fixture finding builds the whole
// security sub-family with the canonical kinds/keys, the occurrence links to
// the vulnerability node_key, the has_finding/occurrence_of/detected_by/
// in_project edges carry the expected endpoints/types, and every ID is
// generation-free (stable across two generations).
func TestSecurityFamilyShape(t *testing.T) {
	meta := graph.Meta{Repo: "org/repo", GroupOrg: "org", VCSType: "gitlab", Branch: "main", SchemaVer: "3", Generation: 7}
	f := securityFixture()

	finding := graph.FindingPoint(meta, f)
	occurrence := graph.VulnerabilityOccurrencePoint(meta, f)
	vuln := graph.VulnerabilityPoint(meta, f)
	scan := graph.SecurityScanPoint(meta, f)
	scanner := graph.VulnerabilityScannerPoint(meta, "gitlab.com", f)
	ident := graph.VulnerabilityIdentifierPoint(meta, "gitlab.com", f.Identifiers[0])

	findingKey := graph.RefFinding("org/repo", "uuid-1")
	occurrenceKey := graph.RefVulnerabilityOccurrence("org/repo", "uuid-1")
	scanKey := graph.RefSecurityScan("org/repo", "9")
	scannerKey := graph.RefVulnerabilityScanner("gitlab.com", "5")

	// (a) node kinds and canonical keys.
	for _, tc := range []struct {
		name string
		p    *qdrant.PointStruct
		kind string
		key  string
	}{
		{"finding", finding, "finding", findingKey},
		{"occurrence", occurrence, "vulnerability_occurrence", occurrenceKey},
		{"vulnerability", vuln, "vulnerability", graph.RefVulnerability("org/repo", "42")},
		{"scan", scan, "security_scan", scanKey},
		{"scanner", scanner, "vulnerability_scanner", scannerKey},
		{"identifier", ident, "vulnerability_identifier", graph.RefVulnerabilityIdentifier("gitlab.com", "CVE-2024-0001")},
	} {
		if got := pointField(tc.p, "kind"); got != tc.kind {
			t.Errorf("%s kind = %q, want %q", tc.name, got, tc.kind)
		}
		if got := pointField(tc.p, "node_key"); got != tc.key {
			t.Errorf("%s node_key = %q, want %q", tc.name, got, tc.key)
		}
	}

	// (b) the occurrence's vulnerability_id IS the vulnerability node_key.
	if got, want := pointField(occurrence, "vulnerability_id"), pointField(vuln, "node_key"); got != want {
		t.Errorf("occurrence vulnerability_id = %q, want the vulnerability node_key %q", got, want)
	}

	// (c) edge endpoints/types.
	for _, tc := range []struct {
		name     string
		p        *qdrant.PointStruct
		et       string
		fromKey  string
		fromType string
		toKey    string
		toType   string
	}{
		{"has_finding scan→finding", graph.HasFindingEdge(meta, scanKey, "security_scan", findingKey, "finding"),
			"has_finding", scanKey, "security_scan", findingKey, "finding"},
		{"has_finding vulnerability→occurrence", graph.HasFindingEdge(meta, graph.RefVulnerability("org/repo", "42"), "vulnerability", occurrenceKey, "vulnerability_occurrence"),
			"has_finding", graph.RefVulnerability("org/repo", "42"), "vulnerability", occurrenceKey, "vulnerability_occurrence"},
		{"occurrence_of", graph.OccurrenceOfEdge(meta, occurrenceKey, graph.RefVulnerability("org/repo", "42")),
			"occurrence_of", occurrenceKey, "vulnerability_occurrence", graph.RefVulnerability("org/repo", "42"), "vulnerability"},
		{"detected_by finding→scanner", graph.DetectedByEdge(meta, findingKey, "finding", scannerKey),
			"detected_by", findingKey, "finding", scannerKey, "vulnerability_scanner"},
		{"detected_by occurrence→scanner", graph.DetectedByEdge(meta, occurrenceKey, "vulnerability_occurrence", scannerKey),
			"detected_by", occurrenceKey, "vulnerability_occurrence", scannerKey, "vulnerability_scanner"},
		{"in_project", graph.InProjectEdge(meta, findingKey, "finding", graph.RefProject("org/repo")),
			"in_project", findingKey, "finding", "org/repo", "project"},
	} {
		if got := pointField(tc.p, "kind"); got != "edge" {
			t.Errorf("%s kind = %q, want edge", tc.name, got)
		}
		for field, want := range map[string]string{
			"edge_type": tc.et,
			"from_key":  tc.fromKey,
			"from_type": tc.fromType,
			"to_key":    tc.toKey,
			"to_type":   tc.toType,
		} {
			if got := pointField(tc.p, field); got != want {
				t.Errorf("%s payload[%s] = %q, want %q", tc.name, field, got, want)
			}
		}
	}

	// (d) node/edge IDs are generation-free: a re-index of the same revision (a
	// later generation) rewrites the same points instead of orphaning them.
	other := meta
	other.Generation = 8
	build := func(m graph.Meta) []*qdrant.PointStruct {
		return []*qdrant.PointStruct{
			graph.FindingPoint(m, f),
			graph.VulnerabilityOccurrencePoint(m, f),
			graph.VulnerabilityPoint(m, f),
			graph.SecurityScanPoint(m, f),
			graph.VulnerabilityScannerPoint(m, "gitlab.com", f),
			graph.VulnerabilityIdentifierPoint(m, "gitlab.com", f.Identifiers[0]),
			graph.OccurrenceOfEdge(m, occurrenceKey, graph.RefVulnerability("org/repo", "42")),
			graph.HasFindingEdge(m, scanKey, "security_scan", findingKey, "finding"),
			graph.DetectedByEdge(m, findingKey, "finding", scannerKey),
			graph.InProjectEdge(m, findingKey, "finding", graph.RefProject("org/repo")),
		}
	}
	a, b := build(meta), build(other)
	for i := range a {
		if a[i].GetId().GetUuid() == "" {
			t.Errorf("point %d has no deterministic UUID id", i)
		}
		if a[i].GetId().GetUuid() != b[i].GetId().GetUuid() {
			t.Errorf("point %d ID is not generation-free: %s != %s",
				i, a[i].GetId().GetUuid(), b[i].GetId().GetUuid())
		}
	}
}
