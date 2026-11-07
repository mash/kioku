// Package runners configures and selects generation runners.
package runners

import (
	"context"
	"errors"
	"slices"
	"time"

	execrunner "github.com/mash/kioku/internal/runner/exec"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/managedagents"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/runner/claudemanaged"
	openairunner "github.com/mash/kioku/internal/runner/openai"
	"github.com/mash/kioku/internal/store"
)

type Config struct {
	Type                 string
	OpenAIAPIKey         string
	OpenAIModel          string
	Command              []string
	DataDir              string
	AnthropicAPIKey      string
	MAAgentID            string
	MAAgentVersion       int64
	MAEnvironmentID      string
	MABaseURL            string
	ManagedAgents        managedagents.Client
	BaseURL              string
	AgentMaxSessions     int
	AgentJobTimeout      time.Duration
	AgentMaxSessionCents int64
}

type Set struct {
	config       Config
	execRunner   *execrunner.Runner
	openaiRunner *openairunner.Runner
	db           *store.Store
	cipher       *auth.Cipher
}

func Load(c Config, db *store.Store, cipher *auth.Cipher, onExit func(string, string)) (*Set, error) {
	if c.AgentMaxSessions == 0 {
		c.AgentMaxSessions = 4
	}
	if c.AgentJobTimeout == 0 {
		c.AgentJobTimeout = time.Hour
	}
	if c.AgentMaxSessionCents == 0 {
		c.AgentMaxSessionCents = 1000
	}
	c.Command = slices.Clone(c.Command)
	s := &Set{config: c, db: db, cipher: cipher}
	if err := s.prepareRunner(onExit); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Set) Close(ctx context.Context) error { return s.execRunner.Close(ctx) }

func (s *Set) jobConnection(job model.Job) (model.AnthropicConnection, error) {
	var out model.AnthropicConnection
	err := s.db.View(func(st model.State) error {
		c := st.Users[job.UserID].Anthropic
		if c == nil || c.ID != job.AnthropicConnectionID {
			return errors.New("Anthropic connection unavailable; reconnect the original workspace")
		}
		out = *c
		return nil
	})
	return out, err
}
func (s *Set) connectionClient(uid string, c model.AnthropicConnection) (*managedagents.API, error) {
	key, err := s.cipher.Open(c.EncryptedKey, "anthropic:"+uid+":"+c.ID)
	if err != nil {
		return nil, errors.New("could not decrypt Anthropic connection")
	}
	return managedagents.New(s.config.MABaseURL, key, nil, c.WorkspaceID), nil
}

// Resolve credentials on each operation so revoked BYOK connections never
// fall back to the operator's key.
func (s *Set) New(job model.Job) (runner.Runner, error) {
	if job.AnthropicConnectionID == "" {
		if job.Run != nil && job.Run.Ref.Resources["provider"] == "openai" {
			if s.openaiRunner == nil {
				return nil, errors.New("OpenAI runner unavailable")
			}
			return s.openaiRunner, nil
		}
		if s.config.Type == "openai" {
			if job.Run != nil && job.Run.Ref.ID != "" {
				return nil, errors.New("cannot use OpenAI credentials for an existing non-OpenAI run")
			}
			return s.openaiRunner, nil
		}
		if s.config.Type == "exec" {
			return s.execRunner, nil
		}
		if s.config.ManagedAgents == nil {
			return nil, runner.ErrNotConfigured
		}
		return claudemanaged.New(func() (managedagents.Client, error) { return s.config.ManagedAgents, nil }, s.config.MAAgentID, s.config.MAAgentVersion, s.config.MAEnvironmentID), nil
	}
	c, err := s.jobConnection(job)
	if err != nil {
		return nil, err
	}
	return claudemanaged.New(func() (managedagents.Client, error) {
		c, err := s.jobConnection(job)
		if err != nil {
			return nil, err
		}
		return s.connectionClient(job.UserID, c)
	}, c.AgentID, c.AgentVersion, c.EnvironmentID), nil
}
