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
