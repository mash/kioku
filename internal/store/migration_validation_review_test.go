package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/mash/kioku/internal/blob"
)

func TestMigrationRejectsMissingCollectionsBeforeMutation(t *testing.T) {
	for _, field := range []string{"inbox", "jobs", "audit", "wiki.files", "views"} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/missing=%t", field, missing), func(t *testing.T) {
				path, objects, cipher, content := migrationFixture(t)
				content.Wiki = &legacyV2Revision{ID: "revision", ProjectID: "p", Files: map[string]blob.Key{}}
				objects.set(t, "projects/p/.kioku/state.json", content)
				key := blob.Key("projects/p/.kioku/state.json")
				if field == "views" {
					key = "projects/p/.kioku/views/index.json"
					objects.set(t, key, map[string]any{"views": map[string]any{}})
				}
				var document map[string]any
				if err := json.Unmarshal(objects.values[key], &document); err != nil {
					t.Fatal(err)
				}
				container, name := document, field
				if field == "wiki.files" {
					container, name = document["wiki"].(map[string]any), "files"
				}
				if missing {
					delete(container, name)
				} else {
					container[name] = nil
				}
				objects.set(t, key, document)
				if err := migrateV2(t.Context(), path, objects, cipher, time.UnixMilli(5000)); err == nil {
					t.Fatal("incomplete legacy document was accepted")
				}
				if objects.puts != 0 || objects.deletes != 0 {
					t.Fatalf("invalid input mutated objects: puts=%d deletes=%d", objects.puts, objects.deletes)
				}
				db, err := catalogConnection(path, false, true)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var version int
				if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
					t.Fatal(version, err)
				}
			})
		}
	}
}

func TestMigrationRejectsMissingMembershipBeforeMutation(t *testing.T) {
	for _, kind := range []string{"organizations", "projects"} {
		t.Run(kind, func(t *testing.T) {
			path, objects, cipher, _ := migrationFixture(t)
			if kind == "organizations" {
				migrationRecord(t, path, kind, "o", legacyV2Organization{ID: "o", Slug: "org"})
			} else {
				migrationRecord(t, path, kind, "p", legacyV2Project{ID: "p", OrgID: "o", Slug: "project"})
			}
			if err := migrateV2(t.Context(), path, objects, cipher, time.UnixMilli(5000)); err == nil {
				t.Fatal("missing membership was migrated as an empty list")
			}
			if objects.puts != 0 || objects.deletes != 0 {
				t.Fatalf("invalid input mutated objects: puts=%d deletes=%d", objects.puts, objects.deletes)
			}
		})
	}
}

func TestMigrationRejectsMissingLivePrincipalsBeforeMutation(t *testing.T) {
	for _, missing := range []string{"project organization", "organization member", "project member"} {
		t.Run(missing, func(t *testing.T) {
			path, objects, cipher, _ := migrationFixture(t)
			project := legacyV2Project{ID: "p", OrgID: "o", Slug: "project", Members: map[string]string{"u": "owner"}}
			switch missing {
			case "project organization":
				project.OrgID = "missing"
			case "organization member":
				migrationRecord(t, path, "organizations", "o", legacyV2Organization{ID: "o", Slug: "org", Members: map[string]string{"missing": "owner"}})
			case "project member":
				project.Members = map[string]string{"missing": "owner"}
			}
			migrationRecord(t, path, "projects", "p", project)
			if err := migrateV2(t.Context(), path, objects, cipher, time.UnixMilli(5000)); err == nil {
				t.Fatal("missing live principal was accepted")
			}
			if objects.puts != 0 || objects.deletes != 0 {
				t.Fatalf("invalid input mutated objects: puts=%d deletes=%d", objects.puts, objects.deletes)
			}
			db, err := catalogConnection(path, false, true)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var version int
			if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
				t.Fatal(version, err)
			}
		})
	}
}

func TestMigrationRejectsMalformedOAuthRequestBeforeMutation(t *testing.T) {
	for _, invalid := range []string{"client", "form", "session", "request id", "client id", "scopes"} {
		for _, kind := range []string{"oauth_codes", "oauth_tokens", "pkce"} {
			t.Run(kind+"/"+invalid, func(t *testing.T) {
				path, objects, cipher, _ := migrationFixture(t)
				request := map[string]any{
					"id": "request", "requestedAt": "2026-01-01T00:00:00Z",
					"client":  map[string]any{"id": "client"},
					"form":    map[string]any{},
					"session": map[string]any{"expires_at": map[string]any{}, "subject": "u"},
				}
				switch invalid {
				case "client":
					request["client"].(map[string]any)["redirect_uris"] = 123
				case "form":
					request["form"] = map[string]any{"redirect_uri": 123}
				case "session":
					request["session"].(map[string]any)["extra"] = []string{"not an object"}
				case "request id":
					delete(request, "id")
				case "client id":
					delete(request["client"].(map[string]any), "id")
				case "scopes":
					request["scopes"] = "invalid"
				}
				raw, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "oauth_codes":
					migrationRecord(t, path, kind, "code", legacyV2OAuthCode{UserID: "u", Active: true, Request: raw, Expires: time.Unix(2000000000, 0), ProjectIDs: []string{"p"}})
				case "oauth_tokens":
					migrationRecord(t, path, kind, "refresh", legacyV2OAuthGrant{UserID: "u", Kind: "refresh", Active: true, Request: raw, ClientID: "client", RequestID: "request", Expires: time.Unix(2000000000, 0), ProjectIDs: []string{"p"}})
				case "pkce":
					valid := json.RawMessage(`{"id":"request","requestedAt":"2026-01-01T00:00:00Z","client":{"id":"client"},"form":{},"session":{"expires_at":{},"subject":"u"}}`)
					migrationRecord(t, path, "oauth_codes", "code", legacyV2OAuthCode{UserID: "u", Active: true, Request: valid, PKCE: raw, Expires: time.Unix(2000000000, 0), ProjectIDs: []string{"p"}})
				}
				if err := migrateV2(t.Context(), path, objects, cipher, time.UnixMilli(5000)); err == nil {
					t.Fatal("malformed OAuth request was migrated and its source deleted")
				}
				if objects.puts != 0 || objects.deletes != 0 {
					t.Fatalf("invalid OAuth request mutated objects: puts=%d deletes=%d", objects.puts, objects.deletes)
				}
				db, err := catalogConnection(path, false, true)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var version int
				if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
					t.Fatal(version, err)
				}
			})
		}
	}
}

func TestMigrationPreservesCompletedWikiRevision(t *testing.T) {
	for _, tc := range []struct {
		name, complete, want string
		incomplete           bool
	}{
		{name: "implicit", want: "wiki-revision"},
		{name: "explicit", complete: "completed-revision", want: "completed-revision"},
		{name: "incomplete without prior", incomplete: true},
		{name: "incomplete with prior", incomplete: true, complete: "prior-revision", want: "prior-revision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, objects, cipher, content := migrationFixture(t)
			content.Incomplete = tc.incomplete
			content.WikiCompleteRevision = tc.complete
			content.Wiki = &legacyV2Revision{ID: "wiki-revision", ProjectID: "p", Files: map[string]blob.Key{}}
			objects.set(t, "projects/p/.kioku/state.json", content)
			if err := migrateV2(t.Context(), path, objects, cipher, time.UnixMilli(5000)); err != nil {
				t.Fatal(err)
			}
			db, err := OpenDatabase(path, false)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.Read(t.Context(), func(q *Queries) error {
				p, err := q.Project("p")
				if err != nil {
					return err
				}
				if got := valueMigrationString(p.Data.WikiCompleteRevision); got != tc.want {
					t.Fatalf("completed Wiki revision = %q, want %q", got, tc.want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
