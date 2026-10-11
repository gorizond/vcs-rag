// Package provider defines a VCS-agnostic interface to GitLab/GitHub
// and normalized metadata types.
package provider

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrNotImplemented is returned by method stubs; implemented in US4.
var ErrNotImplemented = errors.New("not implemented")

// ErrVulnerabilitiesUnavailable marks the security domain as unavailable on
// this provider/instance (T046/F5): a 403/404 or a token missing the security
// scope is a capability signal, not a failure — the caller skips the domain
// instead of failing the pass.
var ErrVulnerabilitiesUnavailable = errors.New("vulnerabilities unavailable")

// ErrDomainUnavailable marks an optionally-gated domain (notes, users, work
// items, milestones, labels, runners; schema v3: group/tree, merge-request
// nodes, security findings) as unavailable on this provider/instance (T005):
// a 403/404 or a token missing the required scope is a capability signal — the
// caller skips the domain instead of failing the pass.
var ErrDomainUnavailable = errors.New("domain unavailable")

// TailLines returns the last n lines of the text (n<=0 — no truncation).
func TailLines(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// VCSProvider is a source of repositories and metadata (FR-003/004).
type VCSProvider interface {
	Type() string // "gitlab" | "github"
	ListRepositories(ctx context.Context) ([]Repo, error)
	ListIssues(ctx context.Context, repo Repo) ([]Issue, error)
	ListChangeRequests(ctx context.Context, repo Repo) ([]ChangeRequest, error)
	ListPipelines(ctx context.Context, repo Repo) ([]Pipeline, error)
	PipelineLogTail(ctx context.Context, repo Repo, pipeline Pipeline, job Job, lines int) (string, error)
}

// Repo is a normalized repository.
type Repo struct {
	ID            string
	Namespace     string
	Path          string
	CloneURL      string
	DefaultBranch string
}

// NormalizeState maps a raw provider state onto the canonical vocabulary
// open|merged|closed|locked|draft (data-model §7, F3): GitLab says "opened",
// GitHub's merged/draft are derived by its provider (needs PR flags).
func NormalizeState(state string) string {
	if state == "opened" {
		return "open"
	}
	return state
}

// NormalizeCIStatus maps a raw CI state onto the canonical vocabulary
// success|failed|running|pending|canceled|skipped (data-model §5, F4). GitLab
// passes the job/pipeline status (conclusion empty); GitHub passes the
// workflow/job (status, conclusion) pair.
func NormalizeCIStatus(status, conclusion string) string {
	switch conclusion {
	case "success":
		return "success"
	case "failure", "timed_out":
		return "failed"
	case "cancelled":
		return "canceled"
	case "skipped", "neutral":
		return "skipped"
	}
	switch status {
	case "success":
		return "success"
	case "failed":
		return "failed"
	case "running", "in_progress":
		return "running"
	case "canceled", "cancelled", "cancelling":
		return "canceled"
	case "skipped":
		return "skipped"
	}
	return "pending"
}

// Issue is an issue/task (GitLab issue = GitHub issue).
type Issue struct {
	ID        int64
	Title     string
	Body      string
	State     string
	URL       string
	UpdatedAt time.Time
	// F3 people/planning fields (additive; data-model §7).
	Author    string
	Assignees []string
	Labels    []string
	Milestone string
}

// ChangeRequest — MR (GitLab) / PR (GitHub).
type ChangeRequest struct {
	ID           int64
	Title        string
	Body         string
	State        string
	SourceBranch string
	TargetBranch string
	URL          string
	UpdatedAt    time.Time
	// F3 people/planning fields (additive; data-model §7).
	Author    string
	Assignees []string
	Reviewers []string
	Labels    []string
	Milestone string
	// Merge-review fields (additive; schema v3, data-model §new nodes): the
	// merge state/commit the merge_request node declares. Zero values keep the
	// pre-parity record unchanged.
	Merged         bool
	MergeCommitSHA string
}

// Pipeline is a CI pipeline with jobs.
type Pipeline struct {
	ID        string
	Status    string
	Ref       string
	SHA       string
	UpdatedAt time.Time
	Jobs      []Job
}

// Job is a pipeline job.
type Job struct {
	ID         string
	Name       string
	Status     string
	Conclusion string
	Duration   time.Duration
}

// RateLimit is the API rate limit state.
type RateLimit struct {
	Remaining  int
	Reset      time.Time
	RetryAfter time.Duration
}

// ChangeRequestFile is a single file diff of a change request (R12).
type ChangeRequestFile struct {
	OldPath   string
	NewPath   string
	Status    string // added | modified | deleted | renamed
	Patch     string
	Truncated bool
	MRID      int
}

// CIPipelineDetail is one job of a CI pipeline with its stage context (R12).
type CIPipelineDetail struct {
	PipelineID int
	Stage      string
	JobName    string
	Status     string
	Ref        string
	CommitSHA  string
	Env        string
	DurationMS int64
}

// DeploymentInfo is a normalized deployment record (R12).
type DeploymentInfo struct {
	ID          int
	Environment string
	Status      string
	Ref         string
	CommitSHA   string
}

// EnvironmentInfo is a normalized CI/CD environment (R12).
type EnvironmentInfo struct {
	Name        string
	State       string
	Tier        string
	ExternalURL string
}

// VulnerabilityFinding is a normalized security finding (R12).
type VulnerabilityFinding struct {
	ID           string
	Severity     string
	Scanner      string
	State        string
	LocationFile string
	// Schema-v3 security family fields (additive; data-model §new nodes): the
	// scanner/identifier/scan context the vulnerability, finding, occurrence,
	// identifier and scanner nodes are derived from. Zero values keep the
	// pre-parity record unchanged.
	UUID                 string
	VulnerabilityID      string
	LocationPath         string
	ScannerKey           string
	ScannerID            string
	ScannerVendor        string
	PrimaryIdentifierKey string
	ScanID               string
	ScanType             string
	ScanStatus           string
	ScanPipelineID       string
	Identifiers          []VulnerabilityIdentifier
}

// VulnerabilityIdentifier is one identifier of a security finding (schema-v3
// vulnerability_identifier node).
type VulnerabilityIdentifier struct {
	ExternalType string
	Name         string
	URL          string
}

// Optional capability interfaces (R12). Implementations may satisfy any
// subset; callers type-assert to detect support.

// ChangeRequestDiffProvider lists file diffs of a change request.
type ChangeRequestDiffProvider interface {
	ListChangeRequestDiffs(ctx context.Context, repo Repo, mrID int) ([]ChangeRequestFile, error)
}

// CIPipelineDetailProvider exposes pipeline jobs, deployments and environments.
type CIPipelineDetailProvider interface {
	ListPipelineJobs(ctx context.Context, repo Repo, pipelineID int) ([]CIPipelineDetail, error)
	ListDeployments(ctx context.Context, repo Repo) ([]DeploymentInfo, error)
	ListEnvironments(ctx context.Context, repo Repo) ([]EnvironmentInfo, error)
}

// VulnerabilityProvider lists security findings of a repository.
type VulnerabilityProvider interface {
	ListVulnerabilities(ctx context.Context, repo Repo) ([]VulnerabilityFinding, error)
}

// Note is a comment/note on an issue or change request (T005; GitLab notes,
// GitHub issue/PR comments — including system/bot notes when the provider
// surfaces them).
type Note struct {
	ID          int64
	ParentKind  string // issue | change_request
	ParentID    int
	AuthorID    string
	AuthorLogin string
	Body        string
	System      bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// User is a normalized user account (T005).
type User struct {
	ID    string
	Login string
	Name  string
	State string
}

// WorkItem is a normalized issue/task (T005); ItemType distinguishes GitLab
// issues from tasks, GitHub always reports "issue".
type WorkItem struct {
	ID          int
	ItemType    string // issue | task
	State       string
	Title       string
	UpdatedAt   time.Time
	AuthorID    string
	AssigneeIDs []string
	LabelNames  []string
	MilestoneID int
}

// Milestone is a normalized milestone (T005).
type Milestone struct {
	ID      int
	Title   string
	State   string
	DueDate *time.Time
}

// Label is a normalized label (T005); Scope is "group"|"project" on GitLab,
// empty where the provider has no scoped labels.
type Label struct {
	Name  string
	Scope string
	Color string
}

// Runner is a normalized CI runner (T005); Online mirrors the provider's
// online/contacted flag.
type Runner struct {
	ID          int
	Description string
	State       string
	RunnerType  string
	Tags        []string
	Online      bool
}

// NotesProvider lists comments/notes of an issue or change request.
type NotesProvider interface {
	ListNotes(ctx context.Context, repo Repo, parentKind string, parentID int) ([]Note, error)
}

// UsersProvider fetches a single user account.
type UsersProvider interface {
	GetUser(ctx context.Context, id string) (User, error)
}

// WorkItemsProvider lists issues/tasks of a repository.
type WorkItemsProvider interface {
	ListWorkItems(ctx context.Context, repo Repo) ([]WorkItem, error)
}

// MilestonesProvider lists milestones of a repository.
type MilestonesProvider interface {
	ListMilestones(ctx context.Context, repo Repo) ([]Milestone, error)
}

// LabelsProvider lists labels of a repository.
type LabelsProvider interface {
	ListLabels(ctx context.Context, repo Repo) ([]Label, error)
}

// RunnersProvider lists CI runners available to a repository.
type RunnersProvider interface {
	ListRunners(ctx context.Context, repo Repo) ([]Runner, error)
}

// Schema-v3 optional domain interfaces (T009; data-model §new nodes). Like the
// capability interfaces above, implementations may satisfy any subset; callers
// type-assert to detect support, and an implementation returns
// ErrDomainUnavailable when the provider/instance lacks the capability
// (403/404 or a token missing the required scope) — a skipped domain, never a
// failed pass.

// TreeEntry is one repository tree entry (structure family); Type is
// "blob" | "tree".
type TreeEntry struct {
	Path string
	Name string
	Type string
}

// Branch is a normalized repository branch (schema-v3 branch node).
type Branch struct {
	Name      string
	Default   bool
	Protected bool
}

// TreeReader lists the repository tree and branches (structure family: the
// directory/file and branch nodes). The tree may be paginated by the provider;
// every entry must be returned.
type TreeReader interface {
	ListTree(ctx context.Context, repo Repo) ([]TreeEntry, error)
	ListBranches(ctx context.Context, repo Repo) ([]Branch, error)
}

// MergeRequestDiffRef is one diff version of a merge request (schema-v3
// merge_request_diff node: the provider's diff_refs / versions triple).
type MergeRequestDiffRef struct {
	Version  string
	BaseSHA  string
	StartSHA string
	HeadSHA  string
	State    string
}

// MergeRequestNodeReader lists change requests with their merge metadata
// (ChangeRequest.Merged/MergeCommitSHA) and the diff versions of one request
// (review family: the merge_request / merge_request_diff nodes). Listing the
// diff refs is per-request — providers fetch versions only on demand.
type MergeRequestNodeReader interface {
	ListMergeRequestNodes(ctx context.Context, repo Repo) ([]ChangeRequest, error)
	ListMergeRequestDiffRefs(ctx context.Context, repo Repo, mrID int) ([]MergeRequestDiffRef, error)
}

// Group is a normalized namespace group (schema-v3 group node).
type Group struct {
	ID       string
	FullPath string
	Path     string
	Name     string
	ParentID string
}

// Project is a normalized project record (schema-v3 project node) with the
// namespace context the node declares (NamespaceKind, CreatorID).
type Project struct {
	ID            string
	FullPath      string
	Path          string
	Name          string
	NamespaceKind string
	CreatorID     string
}

// GroupReader lists the groups and the projects of one group (core family: the
// group/project nodes). Projects are listed per group so their namespace
// (full_path/namespace_kind) context stays complete.
type GroupReader interface {
	ListGroups(ctx context.Context) ([]Group, error)
	ListGroupProjects(ctx context.Context, groupPath string) ([]Project, error)
}

// SecurityVulnReader lists security findings with the scanner, identifier and
// scan context their nodes need (security family: vulnerability, finding,
// occurrence, identifier, scanner, scan). One findings listing carries the
// whole sub-family. A provider without security support returns
// ErrVulnerabilitiesUnavailable (or ErrDomainUnavailable) — the caller skips
// the domain.
type SecurityVulnReader interface {
	ListSecurityFindings(ctx context.Context, repo Repo) ([]VulnerabilityFinding, error)
}
