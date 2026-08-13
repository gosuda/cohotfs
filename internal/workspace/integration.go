package workspace

import "github.com/gosuda/cohotfs/internal/config"

type containerSeedManifest struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Mode        uint32 `json:"mode"`
}

func requestEnvironmentAgents(plan *Plan) config.AgentsSpec {
	return config.AgentsSpec{
		OMP:    config.OMPAgentSpec{Enabled: plan.Integrations["agent:omp"], Config: "seed"},
		Codex:  config.AgentSpec{Enabled: plan.Integrations["agent:codex"], Config: "seed"},
		Claude: config.AgentSpec{Enabled: plan.Integrations["agent:claude"], Config: "seed"},
	}
}

func anyAgentEnabled(agents config.AgentsSpec) bool {
	return agents.OMP.Enabled || agents.Codex.Enabled || agents.Claude.Enabled
}
