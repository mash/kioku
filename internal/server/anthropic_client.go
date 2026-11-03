package server

import (
	"context"
	"errors"
	"github.com/mash/kioku/internal/managedagents"
	"github.com/mash/kioku/internal/model"
)

func (s *Server) hasManagedClient(job model.Job) bool {
	return job.AnthropicConnectionID != "" || s.Config.ManagedAgents != nil
}
func (s *Server) jobConnection(job model.Job) (model.AnthropicConnection, error) {
	var out model.AnthropicConnection
	err := s.DB.View(func(st model.State) error {
		c := st.Users[job.UserID].Anthropic
		if c == nil || c.ID != job.AnthropicConnectionID {
			return errors.New("Anthropic connection unavailable; reconnect the original workspace")
		}
		out = *c
		return nil
	})
	return out, err
}
func (s *Server) connectionClient(uid string, c model.AnthropicConnection) (*managedagents.API, error) {
	key, err := s.Cipher.Open(c.EncryptedKey, "anthropic:"+uid+":"+c.ID)
	if err != nil {
		return nil, errors.New("could not decrypt Anthropic connection")
	}
	return managedagents.New(s.Config.MABaseURL, key, nil, c.WorkspaceID), nil
}

// Resolve credentials on each operation so rotation works during long sessions.
// Missing or revoked BYOK credentials never fall back to the operator's key.
func (s *Server) managedClient(job model.Job) managedagents.Client {
	if job.AnthropicConnectionID == "" {
		return s.Config.ManagedAgents
	}
	return jobManagedClient{resolve: func() (managedagents.Client, error) {
		c, err := s.jobConnection(job)
		if err != nil {
			return nil, err
		}
		return s.connectionClient(job.UserID, c)
	}}
}

type jobManagedClient struct {
	resolve func() (managedagents.Client, error)
}

func (c jobManagedClient) CreateJobVault(ctx context.Context, in managedagents.VaultInput) (string, error) {
	api, err := c.resolve()
	if err != nil {
		return "", err
	}
	return api.CreateJobVault(ctx, in)
}

func (c jobManagedClient) FindJobVaults(ctx context.Context, job, attempt string) ([]string, error) {
	api, err := c.resolve()
	if err != nil {
		return nil, err
	}
	return api.FindJobVaults(ctx, job, attempt)
}

func (c jobManagedClient) ArchiveVault(ctx context.Context, id string) error {
	api, err := c.resolve()
	if err != nil {
		return err
	}
	return api.ArchiveVault(ctx, id)
}

func (c jobManagedClient) CreateSession(ctx context.Context, in managedagents.SessionInput) (string, error) {
	api, err := c.resolve()
	if err != nil {
		return "", err
	}
	return api.CreateSession(ctx, in)
}

func (c jobManagedClient) GetSession(ctx context.Context, id string) (managedagents.Session, error) {
	api, err := c.resolve()
	if err != nil {
		return managedagents.Session{}, err
	}
	return api.GetSession(ctx, id)
}

func (c jobManagedClient) FindSessionByJob(ctx context.Context, job string, attempts ...string) (string, error) {
	api, err := c.resolve()
	if err != nil {
		return "", err
	}
	return api.FindSessionByJob(ctx, job, attempts...)
}

func (c jobManagedClient) Interrupt(ctx context.Context, id string) error {
	api, err := c.resolve()
	if err != nil {
		return err
	}
	return api.Interrupt(ctx, id)
}

func (c jobManagedClient) ArchiveSession(ctx context.Context, id string) error {
	api, err := c.resolve()
	if err != nil {
		return err
	}
	return api.ArchiveSession(ctx, id)
}
