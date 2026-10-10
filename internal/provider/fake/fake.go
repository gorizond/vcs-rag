// Package fake — настраиваемый фейк provider.VCSProvider для тестов.
package fake

import (
	"context"
	"strings"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// Provider реализует provider.VCSProvider поверх заданных данных.
// Ключи карт — Repo.ID (данные) и Job.ID (логи). Поля Err* инжектируют ошибки
// (тесты изоляции, US5). Методы учитывают отмену контекста.
type Provider struct {
	Repos          []provider.Repo
	Issues         map[string][]provider.Issue
	ChangeRequests map[string][]provider.ChangeRequest
	Pipelines      map[string][]provider.Pipeline
	Logs           map[string]string // Job.ID → текст лога

	ErrListRepositories   error
	ErrListIssues         error
	ErrListChangeRequests error
	ErrListPipelines      error
	ErrPipelineLogTail    error
}

// Type возвращает "fake".
func (p *Provider) Type() string { return "fake" }

// ListRepositories возвращает заданные репозитории.
func (p *Provider) ListRepositories(ctx context.Context) ([]provider.Repo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.ErrListRepositories != nil {
		return nil, p.ErrListRepositories
	}
	return p.Repos, nil
}

// ListIssues возвращает issues репозитория.
func (p *Provider) ListIssues(ctx context.Context, repo provider.Repo) ([]provider.Issue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.ErrListIssues != nil {
		return nil, p.ErrListIssues
	}
	return p.Issues[repo.ID], nil
}

// ListChangeRequests возвращает MR/PR репозитория.
func (p *Provider) ListChangeRequests(ctx context.Context, repo provider.Repo) ([]provider.ChangeRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.ErrListChangeRequests != nil {
		return nil, p.ErrListChangeRequests
	}
	return p.ChangeRequests[repo.ID], nil
}

// ListPipelines возвращает пайплайны репозитория.
func (p *Provider) ListPipelines(ctx context.Context, repo provider.Repo) ([]provider.Pipeline, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.ErrListPipelines != nil {
		return nil, p.ErrListPipelines
	}
	return p.Pipelines[repo.ID], nil
}

// PipelineLogTail возвращает хвост лога job — не более lines строк.
func (p *Provider) PipelineLogTail(ctx context.Context, repo provider.Repo, pipeline provider.Pipeline, job provider.Job, lines int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.ErrPipelineLogTail != nil {
		return "", p.ErrPipelineLogTail
	}
	logText := p.Logs[job.ID]
	if logText == "" || lines <= 0 {
		return "", nil
	}
	parts := strings.Split(strings.TrimRight(logText, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n"), nil
}
