// GitLab security findings (T016/US1): the vulnerability_findings listing
// behind the finding, vulnerability_occurrence, vulnerability_scanner,
// vulnerability_identifier and security_scan nodes.
package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// findingReportTypes — the report types the vulnerability_findings endpoint
// accepts. The report_type parameter is required, so the reader walks all of
// them to cover the whole security family.
var findingReportTypes = []string{"sast", "dast", "dependency_scanning", "container_scanning"}

// listFindingsOptions is the vulnerability_findings query: report_type selects
// the scanner class, scope=all returns findings in every occurrence state;
// severity/pipeline_id narrow the listing when set.
type listFindingsOptions struct {
	ReportType string `url:"report_type"`
	Scope      string `url:"scope,omitempty"`
	Severity   string `url:"severity,omitempty"`
	PipelineID int64  `url:"pipeline_id,omitempty"`
	gitlab.ListOptions
}

// rawFinding mirrors one element of the vulnerability_findings response.
type rawFinding struct {
	UUID                string          `json:"uuid"`
	Severity            string          `json:"severity"`
	State               string          `json:"state"`
	VulnerabilityID     int64           `json:"vulnerability_id"`
	PrimaryIdentifierID int64           `json:"primary_identifier_id"`
	Scanner             rawScanner      `json:"scanner"`
	Identifiers         []rawIdentifier `json:"identifiers"`
	Scan                rawScan         `json:"scan"`
	Location            rawLocation     `json:"location"`
}

// rawScanner is the scanner{} context of a finding.
type rawScanner struct {
	ID         int64  `json:"id"`
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
	Vendor     string `json:"vendor"`
}

// rawIdentifier is one entry of the finding's identifiers[] list.
type rawIdentifier struct {
	ExternalType string `json:"external_type"`
	Name         string `json:"name"`
	URL          string `json:"url"`
}

// rawScan is the scan{} context of a finding.
type rawScan struct {
	ID         int64  `json:"id"`
	PipelineID int64  `json:"pipeline_id"`
	ScanType   string `json:"scan_type"`
	Status     string `json:"status"`
}

// rawLocation is the location{} context of a finding.
type rawLocation struct {
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
}

// ListSecurityFindings returns the project security findings (all pages of
// every report type) with the scanner/identifier/scan/location context the
// schema-v3 security nodes need. The endpoint is Ultimate-only: a 403/404
// (missing tier, Security Dashboard or scope) is reported as
// provider.ErrVulnerabilitiesUnavailable, so the caller skips the domain
// instead of failing the pass.
func (c *Client) ListSecurityFindings(ctx context.Context, repo provider.Repo) ([]provider.VulnerabilityFinding, error) {
	pid := repoFullPath(repo)
	path := fmt.Sprintf("projects/%s/vulnerability_findings", gitlab.PathEscape(pid))
	var out []provider.VulnerabilityFinding
	for _, rt := range findingReportTypes {
		opt := &listFindingsOptions{ReportType: rt, Scope: "all", ListOptions: gitlab.ListOptions{PerPage: 100}}
		for {
			req, err := c.api.NewRequest(http.MethodGet, path, opt, []gitlab.RequestOptionFunc{gitlab.WithContext(ctx)})
			if err != nil {
				return nil, fmt.Errorf("gitlab: security findings %s: %w", pid, err)
			}
			var page []rawFinding
			resp, err := c.api.Do(req, &page)
			if err != nil {
				if domainUnavailable(err) {
					return nil, provider.ErrVulnerabilitiesUnavailable
				}
				return nil, fmt.Errorf("gitlab: security findings %s: %w", pid, err)
			}
			for _, f := range page {
				out = append(out, findingFromRaw(f))
			}
			if resp.NextPage == 0 {
				break
			}
			opt.Page = resp.NextPage
		}
	}
	return out, nil
}

// findingFromRaw maps one vulnerability_findings element onto the normalized
// finding: the legacy view (ID/Scanner/LocationFile) plus the schema-v3
// scanner/identifier/scan/location context.
func findingFromRaw(f rawFinding) provider.VulnerabilityFinding {
	ids := make([]provider.VulnerabilityIdentifier, 0, len(f.Identifiers))
	for _, i := range f.Identifiers {
		ids = append(ids, provider.VulnerabilityIdentifier{
			ExternalType: i.ExternalType,
			Name:         i.Name,
			URL:          i.URL,
		})
	}
	// The scanner node key needs a non-empty id; an instance that omits
	// scanner.id falls back to the external id, so scanners never collapse
	// into a single empty-key node.
	scannerID := strconv.FormatInt(f.Scanner.ID, 10)
	if f.Scanner.ID == 0 {
		scannerID = f.Scanner.ExternalID
	}
	return provider.VulnerabilityFinding{
		ID:                   f.UUID,
		Severity:             vulnSeverity(f.Severity),
		Scanner:              f.Scanner.Name,
		State:                f.State,
		LocationFile:         f.Location.File,
		UUID:                 f.UUID,
		VulnerabilityID:      strconv.FormatInt(f.VulnerabilityID, 10),
		LocationPath:         f.Location.File,
		ScannerKey:           f.Scanner.ExternalID,
		ScannerID:            scannerID,
		ScannerVendor:        f.Scanner.Vendor,
		PrimaryIdentifierKey: strconv.FormatInt(f.PrimaryIdentifierID, 10),
		ScanID:               strconv.FormatInt(f.Scan.ID, 10),
		ScanType:             f.Scan.ScanType,
		ScanStatus:           f.Scan.Status,
		ScanPipelineID:       strconv.FormatInt(f.Scan.PipelineID, 10),
		Identifiers:          ids,
	}
}
