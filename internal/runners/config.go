package runners

import (
	"errors"
	"net/url"

	"github.com/mash/kioku/internal/managedagents"
)

func (s *Set) prepareManaged() error {
	c := &s.config
	count := 0
	for _, v := range []string{c.MAAgentID, c.MAEnvironmentID, c.AnthropicAPIKey} {
		if v != "" {
			count++
		}
	}
	if count != 0 && count != 3 {
		return errors.New("ANTHROPIC_API_KEY, KIOKU_MA_AGENT_ID and KIOKU_MA_ENVIRONMENT_ID must be configured together")
	}
	if c.MAAgentVersion < 0 || c.AgentMaxSessions < 1 || c.AgentJobTimeout <= 0 || c.AgentMaxSessionCents < 50 || c.AgentMaxSessionCents > 1_000_000 {
		return errors.New("invalid managed agent limits")
	}
	if count == 3 {
		u, err := url.Parse(c.BaseURL)
		if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("managed generation requires a valid KIOKU_BASE_URL origin")
		}
		if c.ManagedAgents == nil {
			c.ManagedAgents = managedagents.New(c.MABaseURL, c.AnthropicAPIKey, nil)
		}
	}
	return nil
}
