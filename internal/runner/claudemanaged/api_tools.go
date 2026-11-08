package claudemanaged

import "github.com/anthropics/anthropic-sdk-go/v2"

// Every session replaces the agent's tools with this fixed network policy.
var wikiTools = []anthropic.BetaManagedAgentsAgentWithOverridesParamsToolUnion{{
	OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
		DefaultConfig: anthropic.BetaManagedAgentsAgentToolsetDefaultConfigParams{
			PermissionPolicy: anthropic.BetaManagedAgentsAgentToolsetDefaultConfigParamsPermissionPolicyUnion{
				OfAlwaysAllow: &anthropic.BetaManagedAgentsAlwaysAllowPolicyParam{},
			},
		},
		Configs: []anthropic.BetaManagedAgentsAgentToolConfigParamsUnion{
			{OfWebFetch: &anthropic.BetaManagedAgentsWebFetchToolConfigParams{Enabled: anthropic.Bool(false)}},
			{OfWebSearch: &anthropic.BetaManagedAgentsWebSearchToolConfigParams{Enabled: anthropic.Bool(false)}},
		},
	},
}}
