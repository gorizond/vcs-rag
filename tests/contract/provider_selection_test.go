package contract

import (
	"testing"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/provider/factory"
)

// TestFactorySelectsProvider: фабрика выбирает провайдера по конфигу;
// конструктор не делает сетевых вызовов (base URL заведомо недоступен).
func TestFactorySelectsProvider(t *testing.T) {
	gitlabCfg := &config.Config{}
	gitlabCfg.Provider.Type = "gitlab"
	gitlabCfg.Provider.GitLabGroups = []string{"1"}
	gitlabCfg.Provider.GitLabToken = "dummy-token"
	gitlabCfg.Provider.GitLabBaseURL = "http://127.0.0.1:9"
	p, err := factory.New(gitlabCfg)
	if err != nil {
		t.Fatalf("factory.New(gitlab) error: %v", err)
	}
	if p.Type() != "gitlab" {
		t.Errorf("Type() = %q, want gitlab", p.Type())
	}

	githubCfg := &config.Config{}
	githubCfg.Provider.Type = "github"
	githubCfg.Provider.GitHubOrgs = []string{"acme"}
	githubCfg.Provider.GitHubToken = "dummy-token"
	githubCfg.Provider.GitHubBaseURL = "http://127.0.0.1:9/api/v3"
	p, err = factory.New(githubCfg)
	if err != nil {
		t.Fatalf("factory.New(github) error: %v", err)
	}
	if p.Type() != "github" {
		t.Errorf("Type() = %q, want github", p.Type())
	}
}

// TestFactoryUnknownProvider: пустой/неизвестный тип — ошибка (exit 2 — на уровне main).
func TestFactoryUnknownProvider(t *testing.T) {
	if _, err := factory.New(&config.Config{}); err == nil {
		t.Error("factory.New(пустой провайдер) = nil error, ожидалась ошибка")
	}
}
