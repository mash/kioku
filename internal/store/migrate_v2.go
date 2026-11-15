package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
)

// migrationPlan is a transient, validated set of v3 rows, not a runtime State.
// No provider is consulted and no object is mutated until every input passes.
type migrationPlan struct {
	organizations       []OrganizationRow
	organizationMembers [][3]string
	users               []UserRow
	identities          [][3]string
	projects            []ProjectRow
	projectMembers      [][3]string
	sessions            []SessionRow
	invites             []InviteRow
	clients             []OAuthClientRow
	inbox               []InboxRow
	wiki                []WikiFileRow
	jobs                []JobRow
	audit               []AuditRow
	views               []ViewRow
	usage               []UsageRow
	copies              map[blob.Key][]byte
	obsolete            []blob.Key
	sourceRecords       int
	migratedRecords     int
	discardedRecords    int
}
type legacyV2Record struct {
	kind, id string
	body     []byte
}

func migrationString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
func migrationTime(v time.Time) *int64 {
	if v.IsZero() {
		return nil
	}
	n := v.UnixMilli()
	return &n
}
func migrationCreated(v time.Time) int64 {
	if v.IsZero() {
		return 0
	}
	return v.UnixMilli()
}
func migrationOptionalTime(v *time.Time) *int64 {
	if v == nil {
		return nil
	}
	return migrationTime(*v)
}
func decodeLegacyJSON(body []byte, v any) error {
	if !json.Valid(body) || len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] != '{' {
		return errors.New("invalid legacy JSON object")
	}
	if err := json.Unmarshal(body, v); err != nil {
		return errors.New("invalid legacy JSON fields")
	}
	return nil
}
func migrateV2(ctx context.Context, path string, objects blob.Store, cipher *auth.Cipher, now time.Time) error {
	if cipher == nil || objects == nil {
		return errors.New("migration requires existing key and object storage")
	}
	db, err := catalogConnection(path, false, false)
	if err != nil {
		return err
	}
	defer db.Close()
	var version, pending int
	var check string
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 2 {
		return errors.New("stopped migration requires schema 2; restore verified DB, objects and key")
	}
	if err = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return errors.New("SQLite integrity check failed")
	}
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM pending").Scan(&pending); err != nil {
		return err
	}
	if pending != 0 {
		return errors.New("unfinished transaction; complete v2 recovery before migration")
	}
	var unexpected int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT IN ('records','pending','usage_ledger') AND name NOT LIKE 'sqlite_%'").Scan(&unexpected); err != nil {
		return err
	}
	if unexpected != 0 {
		return errors.New("unexpected legacy catalog tables")
	}
	rows, err := db.QueryContext(ctx, "SELECT kind,id,payload FROM records ORDER BY kind,id")
	if err != nil {
		return err
	}
	records := []legacyV2Record{}
	for rows.Next() {
		var r legacyV2Record
		if err = rows.Scan(&r.kind, &r.id, &r.body); err != nil {
			rows.Close()
			return err
		}
		records = append(records, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	plan := &migrationPlan{copies: map[blob.Key][]byte{}, sourceRecords: len(records)}
	if err = plan.readRecords(ctx, records, objects, cipher, now); err != nil {
		return err
	}
	if plan.sourceRecords != plan.migratedRecords+plan.discardedRecords {
		return errors.New("legacy record count mismatch")
	}
	// Exercise every constraint and verify row counts before the first PUT. This
	// temporary in-memory database is discarded, including on validation failure.
	validation, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return err
	}
	validation.SetMaxOpenConns(1)
	err = transact(ctx, validation, true, nil, plan.apply)
	closeErr := validation.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return fmt.Errorf("migration validation failed: %w", err)
	}
	for _, key := range slices.Sorted(maps.Keys(plan.copies)) {
		if err = objects.Put(ctx, key, plan.copies[key]); err != nil {
			return err
		}
	}
	if err = transact(ctx, db, true, nil, plan.apply); err != nil {
		return err
	}
	for _, key := range plan.obsolete {
		if err = objects.Delete(ctx, key); err != nil {
			return fmt.Errorf("migration cleanup failed; restore verified DB, objects and key: %w", err)
		}
	}
	return transact(ctx, db, true, nil, func(q *Queries) error {
		_, e := q.exec(`UPDATE schema_state SET data='{"ready":true}' WHERE id=1`)
		return e
	})
}
func (p *migrationPlan) apply(q *Queries) error {
	if _, err := q.exec(catalogSchema); err != nil {
		return err
	}
	if _, err := q.exec(`INSERT INTO schema_state VALUES(1,'{"ready":false}')`); err != nil {
		return err
	}
	for _, v := range p.organizations {
		if e := q.PutOrganization(v); e != nil {
			return e
		}
	}
	for _, v := range p.organizationMembers {
		if e := q.PutOrganizationMember(v[0], v[1], v[2]); e != nil {
			return e
		}
	}
	for _, v := range p.users {
		if e := q.PutUser(v); e != nil {
			return e
		}
	}
	for _, v := range p.identities {
		if e := q.PutIdentity(v[0], v[1], v[2]); e != nil {
			return e
		}
	}
	for _, v := range p.projects {
		if e := q.PutProject(v); e != nil {
			return e
		}
	}
	for _, v := range p.projectMembers {
		if e := q.PutProjectMember(v[0], v[1], v[2]); e != nil {
			return e
		}
	}
	for _, v := range p.sessions {
		if e := q.InsertSession(v); e != nil {
			return e
		}
	}
	for _, v := range p.invites {
		if e := q.PutInvite(v); e != nil {
			return e
		}
	}
	for _, v := range p.clients {
		if e := q.PutOAuthClient(v); e != nil {
			return e
		}
	}
	for _, v := range p.inbox {
		if e := q.PutInbox(v); e != nil {
			return e
		}
	}
	for _, v := range p.wiki {
		if e := q.PutWikiFile(v); e != nil {
			return e
		}
	}
	for _, v := range p.jobs {
		if e := q.PutJob(v); e != nil {
			return e
		}
	}
	for _, v := range p.audit {
		if e := q.PutAudit(v); e != nil {
			return e
		}
	}
	for _, v := range p.views {
		if e := q.PutView(v); e != nil {
			return e
		}
	}
	for _, v := range p.usage {
		if e := q.RecordUsage(v); e != nil {
			return e
		}
	}
	counts := map[string]int{"organizations": len(p.organizations), "organization_members": len(p.organizationMembers), "users": len(p.users), "user_identities": len(p.identities), "projects": len(p.projects), "project_members": len(p.projectMembers), "sessions": len(p.sessions), "invites": len(p.invites), "oauth_clients": len(p.clients), "inbox": len(p.inbox), "wiki_files": len(p.wiki), "jobs": len(p.jobs), "audit": len(p.audit), "views": len(p.views), "usage": len(p.usage)}
	for table, want := range counts {
		var n int
		if e := q.row("SELECT count(*) FROM " + table).Scan(&n); e != nil {
			return e
		}
		if n != want {
			return fmt.Errorf("migration row count mismatch: %s", table)
		}
	}
	for _, project := range p.projects {
		if e := q.CompactHistory(project.ID, nil); e != nil {
			return e
		}
	}
	var usages int
	if e := q.row("SELECT count(*) FROM usage").Scan(&usages); e != nil {
		return e
	}
	if usages != len(p.usage) {
		return errors.New("migration history lost usage")
	}
	_, err := q.exec("DROP TABLE IF EXISTS records; DROP TABLE IF EXISTS pending; DROP TABLE IF EXISTS usage_ledger; PRAGMA user_version=3")
	return err
}
