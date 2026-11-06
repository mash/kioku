package managedagents

import (
	"context"
	"errors"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/param"
	"github.com/mash/kioku/prompts"
	"gopkg.in/yaml.v3"
)

// EnsureResources recovers resources after a lost create response using a stable
// connection tag. Save each ID before creating the next resource.
func (a *API) EnsureResources(ctx context.Context, connection, host, agentID, environmentID string, save func(string, int64, string) error) error {
	var version int64
	if agentID == "" {
		list := a.sdk.Agents.ListAutoPaging(ctx, anthropic.BetaAgentListParams{})
		for list.Next() {
			v := list.Current()
			if v.Metadata["kioku_connection"] == connection && v.ArchivedAt.IsZero() {
				agentID, version = v.ID, v.Version
				break
			}
		}
		if err := list.Err(); err != nil {
			return apiError(err)
		}
	}
	if agentID == "" {
		parts := strings.SplitN(prompts.WikiDefinition, "---", 3)
		if len(parts) != 3 {
			return errors.New("invalid embedded agent definition")
		}
		var definition map[string]any
		if err := yaml.Unmarshal([]byte(parts[1]), &definition); err != nil {
			return errors.New("invalid agent definition")
		}
		definition["system"] = strings.TrimSpace(parts[2])
		definition["metadata"] = map[string]string{"kioku_connection": connection}
		// SDK parameter structs are write-only; unmarshalling into them silently
		// drops model shorthand and tool overrides. Preserve the manifest body.
		params := param.Override[anthropic.BetaAgentNewParams](definition)
		agent, err := a.sdk.Agents.New(ctx, params)
		if err != nil {
			return apiError(err)
		}
		agentID, version = agent.ID, agent.Version
	} else {
		agent, err := a.sdk.Agents.Get(ctx, agentID, anthropic.BetaAgentGetParams{})
		if err != nil {
			return apiError(err)
		}
		if !agent.ArchivedAt.IsZero() {
			return errors.New("Anthropic agent is archived")
		}
		version = agent.Version
	}
	if agentID == "" || version < 1 {
		return errors.New("invalid Anthropic agent response")
	}
	if err := save(agentID, version, environmentID); err != nil {
		return err
	}
	if environmentID == "" {
		list := a.sdk.Environments.ListAutoPaging(ctx, anthropic.BetaEnvironmentListParams{})
		for list.Next() {
			v := list.Current()
			if v.Metadata["kioku_connection"] == connection && v.ArchivedAt == "" {
				environmentID = v.ID
				break
			}
		}
		if err := list.Err(); err != nil {
			return apiError(err)
		}
	}
	if environmentID == "" {
		params := param.Override[anthropic.BetaEnvironmentNewParams](map[string]any{"name": "kioku-wiki", "metadata": map[string]string{"kioku_connection": connection}, "config": map[string]any{"type": "cloud", "networking": map[string]any{"type": "limited", "allowed_hosts": []string{host}, "allow_package_managers": false, "allow_mcp_servers": false}}})
		env, err := a.sdk.Environments.New(ctx, params)
		if err != nil {
			return apiError(err)
		}
		environmentID = env.ID
	} else {
		env, err := a.sdk.Environments.Get(ctx, environmentID, anthropic.BetaEnvironmentGetParams{})
		if err != nil {
			return apiError(err)
		}
		if env.ArchivedAt != "" {
			return errors.New("Anthropic environment is archived")
		}
	}
	if environmentID == "" {
		return errors.New("invalid Anthropic environment response")
	}
	return save(agentID, version, environmentID)
}

func (a *API) ValidateResources(ctx context.Context, agentID, environmentID string) error {
	agent, err := a.sdk.Agents.Get(ctx, agentID, anthropic.BetaAgentGetParams{})
	if err != nil {
		return apiError(err)
	}
	if !agent.ArchivedAt.IsZero() {
		return errors.New("Anthropic agent is archived")
	}
	env, err := a.sdk.Environments.Get(ctx, environmentID, anthropic.BetaEnvironmentGetParams{})
	if err != nil {
		return apiError(err)
	}
	if env.ArchivedAt != "" {
		return errors.New("Anthropic environment is archived")
	}
	return nil
}
