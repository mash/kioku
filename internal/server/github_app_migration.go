package server

import "github.com/mash/kioku/internal/model"

// The source App has its own record kinds. Retire broad OAuth source credentials
// once without deleting login identities or sessions. GitHub's remote OAuth
// grant still needs explicit revocation by the user.
func (s *Server) migrateGitHubSources() error {
	return s.DB.Update(func(st *model.State) error {
		metrics := st.GitHubMetrics["cumulative"]
		if metrics.MigrationComplete {
			return nil
		}
		for id, u := range st.Users {
			delete(u.Connections, "github")
			st.Users[id] = u
		}
		for id, d := range st.Devices {
			if d.IdentityProvider != "github" {
				continue
			}
			if d.Purpose == "connect" {
				delete(st.Devices, id)
			} else {
				d.IdentityCredential = ""
				st.Devices[id] = d
			}
		}
		for id, pending := range st.OAuth {
			if pending.Provider == "github" && pending.Purpose == "connect" {
				delete(st.OAuth, id)
			}
		}
		for id, session := range st.Sessions {
			if session.IdentityProvider == "github" {
				session.IdentityCredential = ""
				st.Sessions[id] = session
			}
		}
		metrics.MigrationComplete = true
		st.GitHubMetrics["cumulative"] = metrics
		return nil
	})
}
