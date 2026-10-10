// Package factory — сборка provider.VCSProvider по конфигурации (T020).
//
// Вынесен отдельным пакетом: положить New в package provider невозможно —
// provider импортировал бы provider/gitlab|github, а те используют типы
// контракта (цикл импортов). Конструкторы сетевых вызовов не делают.
package factory

import (
	"fmt"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/provider"
	"github.com/gorizond/vcs-rag/internal/provider/github"
	"github.com/gorizond/vcs-rag/internal/provider/gitlab"
)

// New создаёт VCSProvider по конфигурации. Сетевых вызовов не выполняет.
func New(cfg *config.Config) (provider.VCSProvider, error) {
	switch cfg.Provider.Type {
	case "gitlab":
		return gitlab.New(cfg)
	case "github":
		return github.New(cfg)
	default:
		return nil, fmt.Errorf("неизвестный VCS-провайдер %q", cfg.Provider.Type)
	}
}
