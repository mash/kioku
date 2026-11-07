package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
)

func githubConfig(purpose string) (oauth2.Endpoint, []string) {
	scopes := []string{"read:user", "user:email"}
	if purpose == "connect" {
		scopes = append(scopes, "repo")
	}
	return github.Endpoint, scopes
}

func (p Provider) githubIdentity(ctx context.Context, token string) (Identity, error) {
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := p.get(ctx, "https://api.github.com/user", token, &u); err != nil {
		return Identity{}, err
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := p.get(ctx, "https://api.github.com/user/emails", token, &emails); err != nil {
		return Identity{}, err
	}
	if u.ID == 0 {
		return Identity{}, errors.New("invalid GitHub user")
	}
	if u.Name == "" {
		u.Name = u.Login
	}
	for _, e := range emails {
		if e.Primary && e.Verified {
			return Identity{fmt.Sprint(u.ID), u.Name, strings.ToLower(e.Email)}, nil
		}
	}
	return Identity{}, errors.New("a verified primary GitHub email is required")
}
