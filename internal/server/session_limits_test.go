package server

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"
)

// fillSessions inserts n rows of one kind for another user, as if many users
// or one attacker had filled the table.
func fillSessions(t *testing.T, f *fixture, user, kind, status, data string, n int) {
	t.Helper()
	fixtureSQL(t, f, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<`+strconv.Itoa(n)+`)
		INSERT INTO sessions(id,kind,status,user_id,project_id,created,expires,data)
		SELECT '`+user+`-`+kind+`-'||x,'`+kind+`','`+status+`','`+user+`',NULL,0,9999999999999,'`+data+`' FROM n`)
}

func sessionCount(t *testing.T, f *fixture, user, kind string) int {
	t.Helper()
	var n int
	fixtureCheck(t, fixtureSQLDatabase(t, f).QueryRow("SELECT count(*) FROM sessions WHERE user_id=? AND kind=?", user, kind).Scan(&n))
	return n
}

func TestBrowserLoginWorksWhenSessionTableIsFullAndBoundsEachUser(t *testing.T) {
	t.Parallel()
	f := setupLoopback(t)
	fillSessions(t, f, "other", "user", "active", "{}", 10000)
	login := func() {
		t.Helper()
		resp := finishLoopbackOAuth(t, f, startLoopbackOAuth(t, f, loopbackAttempt{startURL: f.url + "/auth/google/start"}))
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("browser login: %d", resp.StatusCode)
		}
	}
	login()
	var uid string
	fixtureCheck(t, fixtureSQLDatabase(t, f).QueryRow("SELECT user_id FROM sessions WHERE kind='user' AND user_id<>'other'").Scan(&uid))
	fillSessions(t, f, uid, "user", "active", "{}", 60)
	login()
	if n := sessionCount(t, f, uid, "user"); n != 50 {
		t.Fatalf("sessions of one user are not bounded: %d", n)
	}
	var newest int
	fixtureCheck(t, fixtureSQLDatabase(t, f).QueryRow("SELECT count(*) FROM sessions WHERE user_id=? AND kind='user' AND created>0", uid).Scan(&newest))
	if newest != 2 {
		t.Fatalf("the eviction removed new sessions instead of the oldest: %d", newest)
	}
	if n := sessionCount(t, f, "other", "user"); n != 10000 {
		t.Fatalf("a login removed sessions of another user: %d", n)
	}
}

func TestConnectorRefreshWorksWhenSessionTableIsFullAndPruneDropsOldRevokedTokens(t *testing.T) {
	t.Parallel()
	f := setupWithWorker(t, false)
	client := connectorClient(t, f)
	human := f.login("owner@example.com")
	_, form := connectorAuthCode(t, f, client, human, securityProject(t, f, human).ID)
	_, refresh := connectorTokenPair(t, f, form)
	var uid string
	fixtureCheck(t, fixtureSQLDatabase(t, f).QueryRow("SELECT user_id FROM sessions WHERE kind='mcp' LIMIT 1").Scan(&uid))
	fillSessions(t, f, "other", "mcp", "revoked", "{}", 10000)
	fillSessions(t, f, uid, "mcp", "revoked", `{"token_kind":"refresh","request_id":"old"}`, 1000)
	for range 2 {
		_, refresh = connectorTokenPair(t, f, url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {refresh}})
	}
	if n := sessionCount(t, f, uid, "mcp"); n != 1000 {
		t.Fatalf("connector tokens of one user are not bounded: %d", n)
	}
	fixtureSQL(t, f, `INSERT INTO oauth_clients(client_id,data) VALUES('unused','{"created":0,"client_name":"","redirect_uris":[]}')`)
	fixtureCheck(t, f.s.storage.pruneExpired(t.Context()))
	if n := sessionCount(t, f, "other", "mcp"); n != 0 {
		t.Fatalf("prune kept old revoked tokens: %d", n)
	}
	var clients int
	fixtureCheck(t, fixtureSQLDatabase(t, f).QueryRow("SELECT count(*) FROM oauth_clients WHERE client_id IN (?, 'unused')", client).Scan(&clients))
	if clients != 1 {
		t.Fatalf("prune must delete only the unused old client: %d", clients)
	}
	connectorTokenPair(t, f, url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {refresh}})
}

func TestConnectorRegistrationIsLimitedPerAddressNotGlobally(t *testing.T) {
	t.Parallel()
	f := setupWithWorker(t, false)
	fixtureSQL(t, f, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10000)
		INSERT INTO oauth_clients(client_id,data) SELECT 'client-'||x,'{"created":1}' FROM n`)
	body := map[string]any{"redirect_uris": []string{f.url + "/callback"}}
	for i := range 11 {
		status := f.request("POST", "/oauth/register", "", body, nil)
		if want := map[bool]int{true: 201, false: 429}[i < 10]; status != want {
			t.Fatalf("registration %d: %d, want %d", i, status, want)
		}
	}
	other := connectorForm(f, "POST", "/oauth/register", "", nil)
	if other.Code == http.StatusTooManyRequests {
		t.Fatal("one address blocked registration from another address")
	}
}
