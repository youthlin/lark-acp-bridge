package acp

import (
	"maps"
	"sort"

	"github.com/youthlin/lark-acp-bridge/internal/config"
)

type Registry struct {
	agents map[string]config.AgentConfig
}

func NewRegistry(agents map[string]config.AgentConfig) *Registry {
	copied := make(map[string]config.AgentConfig, len(agents))
	maps.Copy(copied, agents)
	return &Registry{agents: copied}
}

func (r *Registry) Get(name string) (config.AgentConfig, bool) {
	agent, ok := r.agents[name]
	return agent, ok
}

func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.agents))
	for name := range r.agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
