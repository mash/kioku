package store

import (
	"path/filepath"
	"testing"
)

func TestInboxSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	d, err := OpenDatabase(path, true)
	if err != nil {
		t.Fatal(err)
	}
	catalogWrite(t, d, func(q *Queries) error {
		return q.PutInbox(InboxRow{ID: "i", ProjectID: "p", SourceURL: "https://example.com/a", RunID: pointer("job"), Remove: true, Data: InboxData{Submitter: "u"}})
	})
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = OpenDatabase(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Read(t.Context(), func(q *Queries) error {
		rows, e := q.ProjectInbox("p")
		if len(rows) != 1 || rows[0].RunID == nil || *rows[0].RunID != "job" || !rows[0].Remove || rows[0].Data.Submitter != "u" {
			t.Fatal(rows)
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
}
func TestInboxReplacementKeepsAssignedAttempt(t *testing.T) {
	d := newCatalog(t)
	catalogWrite(t, d, func(q *Queries) error {
		for _, id := range []string{"running", "waiting"} {
			row := InboxRow{ID: id, ProjectID: "p", SourceURL: "https://example.com/a"}
			if id == "running" {
				row.RunID = pointer("job")
			}
			if e := q.PutInbox(row); e != nil {
				return e
			}
		}
		return q.ReplaceInbox(InboxRow{ID: "replacement", ProjectID: "p", SourceURL: "https://example.com/a", Remove: true})
	})
	if e := d.Read(t.Context(), func(q *Queries) error {
		rows, e := q.ProjectInbox("p")
		if len(rows) != 2 {
			t.Fatal(rows)
		}
		row, e := q.Inbox("replacement")
		if !row.Remove || row.RunID != nil {
			t.Fatal(row)
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
	catalogWrite(t, d, func(q *Queries) error { return q.ReleaseInbox("p", "job") })
	if e := d.Read(t.Context(), func(q *Queries) error {
		rows, e := q.ProjectInbox("p")
		if len(rows) != 1 || rows[0].ID != "replacement" {
			t.Fatal(rows)
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
}
