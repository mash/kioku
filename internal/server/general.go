package server

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

func (s *Server) EnsureGeneralWikis(ctx context.Context) error {
	return s.Store.Write(ctx, func(q *store.Queries) error {
		users, err := q.UsersWithoutGeneral()
		if err != nil {
			return err
		}
		for _, user := range users {
			if err := ensureGeneralProject(q, user.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func ensureGeneralProject(q *store.Queries, uid string) error {
	if _, err := q.GeneralProject(uid); !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	created := time.Now().UnixMilli()
	org := store.OrganizationRow{
		ID: store.GeneralOrganizationID(uid), Slug: "u-" + auth.Hash(uid)[:12] + "-general",
		Data: store.OrganizationData{Name: "General", Created: created},
	}
	if err := q.PutOrganization(org); err != nil {
		return err
	}
	row := store.ProjectRow{
		ID: auth.Token()[:20], OrgID: org.ID, Slug: store.GeneralProjectSlug, Created: created,
		Data: store.ProjectData{Name: "General"},
	}
	if err := q.PutProject(row); err != nil {
		return err
	}
	return q.PutProjectMember(row.ID, uid, "owner")
}
