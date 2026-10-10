// Package provider описывает VCS-агностичный интерфейс к GitLab/GitHub
// и нормализованные типы метаданных.
package provider

import (
	"context"
	"errors"
	"time"
)

// ErrNotImplemented возвращают заглушки методов; реализация — US4.
var ErrNotImplemented = errors.New("not implemented")

// VCSProvider — источник репозиториев и метаданных (FR-003/004).
type VCSProvider interface {
	Type() string // "gitlab" | "github"
	ListRepositories(ctx context.Context) ([]Repo, error)
	ListIssues(ctx context.Context, repo Repo) ([]Issue, error)
	ListChangeRequests(ctx context.Context, repo Repo) ([]ChangeRequest, error)
	ListPipelines(ctx context.Context, repo Repo) ([]Pipeline, error)
	PipelineLogTail(ctx context.Context, repo Repo, pipeline Pipeline, job Job, lines int) (string, error)
}

// Repo — нормализованный репозиторий.
type Repo struct {
	ID            string
	Namespace     string
	Path          string
	CloneURL      string
	DefaultBranch string
}

// Issue — issue/задача (GitLab issue = GitHub issue).
type Issue struct {
	ID        int64
	Title     string
	Body      string
	State     string
	URL       string
	UpdatedAt time.Time
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
}

// Pipeline — пайплайн CI с задачами.
type Pipeline struct {
	ID        string
	Status    string
	Ref       string
	SHA       string
	UpdatedAt time.Time
	Jobs      []Job
}

// Job — задача пайплайна.
type Job struct {
	ID         string
	Name       string
	Status     string
	Conclusion string
	Duration   time.Duration
}

// RateLimit — состояние лимитов API.
type RateLimit struct {
	Remaining  int
	Reset      time.Time
	RetryAfter time.Duration
}
