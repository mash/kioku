package cli

import (
	"errors"
	"os"
)

// logout revokes the credential that Run selected. A 401 means the server
// already ended it, so logout still clears the local copy.
func (a *App) logout() error {
	if a.kind == credentialNone {
		return errors.New("not logged in")
	}
	if status, err := a.request("POST", "/api/logout", nil, nil); err != nil && status != 401 {
		return err
	}
	if a.kind == credentialAgent {
		if err := os.Remove(a.storedAgentPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	a.Config.Token = ""
	return a.saveCredentials()
}
