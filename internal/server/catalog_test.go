package server

import (
	"testing"
)

func TestOAuthSQLFailureRollsBackCodeExchange(t *testing.T) {
	t.Parallel()
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	p := securityProject(t, f, human)
	client := connectorClient(t, f)
	_, form := connectorAuthCode(t, f, client, human, p.ID)
	restore := fixtureSQLFault(t, f, "sessions", "INSERT", "NEW.kind='mcp'")
	if response := connectorForm(f, "POST", "/oauth/token", "", form); response.Code != 500 {
		t.Fatal("SQL failure disguised as protocol rejection", response.Code, response.Body.String())
	}
	restore()
	if response := connectorForm(f, "POST", "/oauth/token", "", form); response.Code != 200 {
		t.Fatal("SQL failure consumed code", response.Code, response.Body.String())
	}
}
