package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mash/kioku/internal/connector"
	"golang.org/x/oauth2"
	oauthprovider "golang.org/x/oauth2/github"
)

func (Connector) OAuth(purpose, verifier string) connector.OAuth {
	// GitHub App permissions come from its registration, not OAuth scopes.
	endpoint := oauthprovider.Endpoint
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	return connector.OAuth{Endpoint: endpoint}
}

func (p Connector) Identity(ctx context.Context, get connector.Get, token string) (connector.Identity, error) {
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := get(ctx, "https://api.github.com/user", token, &u); err != nil {
		return connector.Identity{}, err
	}
	if u.ID == 0 {
		return connector.Identity{}, errors.New("invalid GitHub user")
	}
	if u.Name == "" {
		u.Name = u.Login
	}
	// The primary address is not guaranteed to appear on the first page.
	for page := 1; ; page++ {
		var emails []struct {
			Email    string `json:"email"`
			Primary  bool   `json:"primary"`
			Verified bool   `json:"verified"`
		}
		if err := get(ctx, fmt.Sprintf("https://api.github.com/user/emails?per_page=100&page=%d", page), token, &emails); err != nil {
			return connector.Identity{}, err
		}
		for _, e := range emails {
			if e.Primary && e.Verified {
				return connector.Identity{ID: fmt.Sprint(u.ID), Name: u.Name, Email: strings.ToLower(e.Email)}, nil
			}
		}
		if len(emails) < 100 {
			break
		}
	}
	return connector.Identity{}, errors.New("a verified primary GitHub email is required")
}

func (p Connector) LoginIdentity(ctx context.Context, get connector.Get, clientID string, token *oauth2.Token, verifier string) (connector.Identity, error) {
	return p.Identity(ctx, get, token.AccessToken)
}

type Connector struct{}

var _ connector.Connector = Connector{}
