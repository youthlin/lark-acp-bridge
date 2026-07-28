package acp

import (
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

type Registry struct {
	byName map[string]config.AgentConfig
	names  []string
}

func NewRegistry(cfg config.Config) *Registry {
	byName := config.AgentMap(cfg.AgentList)
	names := make([]string, 0, len(byName))
	seen := make(map[string]struct{}, len(byName))
	for _, agent := range cfg.AgentList {
		if _, ok := byName[agent.Name]; !ok {
			continue
		}
		if _, ok := seen[agent.Name]; ok {
			continue
		}
		seen[agent.Name] = struct{}{}
		names = append(names, agent.Name)
	}
	return &Registry{byName: byName, names: names}
}

func (r *Registry) Get(name string) (config.AgentConfig, bool) {
	agent, ok := r.byName[name]
	return agent, ok
}

func (r *Registry) Names() []string {
	names := append([]string(nil), r.names...)
	return names
}
