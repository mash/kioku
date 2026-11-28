package server

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

type signupData struct {
	store.IdentitySessionData
	Email      string `json:"email"`
	Name       string `json:"name"`
	ReturnPath string `json:"return_path"`
	Device     string `json:"device"`
}

func defaultUserSlug(email string) string {
	local, _, _ := strings.Cut(email, "@")
	slug := normalizeSlug(local)
	if !validSlug(slug) {
		return "user"
	}
	if reservedNamespace(slug) {
		return normalizeSlug("user-" + slug)
	}
	return slug
}

func (s *Server) setSignupCookie(w http.ResponseWriter, signup string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: "kioku_signup", Value: signup, Path: "/auth/signup", MaxAge: maxAge, HttpOnly: true, Secure: strings.HasPrefix(s.Config.BaseURL, "https://"), SameSite: http.SameSiteLaxMode})
}

func (s *Server) beginSignup(q *store.Queries, token string, pending auth.OAuthCookie, identity auth.Identity, credential auth.Credential) error {
	uid := auth.Token()[:20]
	raw, err := store.EncodeSessionData(signupData{
		IdentitySessionData: identityData(s.Cipher, credential, uid, pending.Provider, identity.ID),
		Email:               identity.Email, Name: identity.Name, ReturnPath: store.ValueString(pending.ReturnPath), Device: store.ValueString(pending.CLILogin),
	})
	if err != nil {
		return err
	}
	now := time.Now()
	return q.InsertSession(store.SessionRow{ID: auth.Hash(token), Kind: "signup", Status: "pending", UserID: uid, Created: now.UnixMilli(), Expires: now.Add(10 * time.Minute).UnixMilli(), Data: raw})
}

const signupTemplate = `{{define "body"}}<h1>アカウントを作成</h1>
<p>ユーザースラッグは個人組織の URL に使います。登録後は変更できません。</p>
{{if .Error}}<p role="alert">{{.Error}}</p>{{end}}
<form method="post" action="/auth/signup">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<label>ユーザースラッグ <input name="slug" type="text" value="{{.Slug}}" required maxlength="63" pattern="[a-z0-9]+(-[a-z0-9]+)*" autocapitalize="none" spellcheck="false"></label>
<p><button type="submit">アカウントを作成</button></p>
</form>{{end}}`

func signupPage(w http.ResponseWriter, status int, csrf, slug, problem, loopbackOrigin string) error {
	// A form POST needs a same-origin Origin and may redirect to the registered CLI listener.
	w.Header().Set("Referrer-Policy", "same-origin")
	if loopbackOrigin != "" {
		w.Header().Set("Content-Security-Policy", pageCSP+" "+loopbackOrigin)
	}
	return renderStatus(w, status, "アカウントを作成", struct{ CSRF, Slug, Error string }{csrf, slug, problem}, signupTemplate)
}

func (s *Server) signup(w http.ResponseWriter, r *http.Request, _ string) error {
	cookie, err := r.Cookie("kioku_signup")
	if err != nil {
		return fail(403, "signup expired; restart login")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err = r.ParseForm(); err != nil {
		return fail(400, "invalid signup form")
	}
	id := auth.Hash(cookie.Value)
	if subtle.ConstantTimeCompare([]byte(id), []byte(r.PostForm.Get("csrf"))) != 1 {
		return fail(403, "signup state mismatch; restart login")
	}
	slug := r.PostForm.Get("slug")
	var data signupData
	var completed oauthCompletion
	var loopbackOrigin string
	err = s.Store.Write(r.Context(), func(q *store.Queries) error {
		now := time.Now()
		row, err := q.TakeSession(id, "signup", now.UnixMilli())
		if errors.Is(err, sql.ErrNoRows) {
			return fail(410, "signup expired; restart login")
		}
		if err != nil {
			return err
		}
		data, err = store.SessionData[signupData](row)
		if err != nil {
			return err
		}
		if _, err := authorizeReturn(data.ReturnPath); err != nil {
			return err
		}
		var login store.SessionRow
		var loginData store.CLILoginData
		if data.Device != "" {
			var ok bool
			login, loginData, ok, err = pendingLoopback(q, data.Device)
			if err != nil {
				return err
			}
			if !ok {
				return errLoopbackExpired
			}
			loopbackOrigin = redirectOrigin(store.ValueString(loginData.LoopbackRedirectURI))
		}
		provider, identity := store.ValueString(data.IdentityProvider), store.ValueString(data.IdentityID)
		if _, err = q.UserByIdentity(provider, identity); err == nil {
			return fail(409, "このログイン ID は登録済みです。ログインし直してください。")
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !validSlug(slug) || reservedNamespace(slug) {
			return fail(400, "ユーザースラッグには半角小文字、数字、単独のハイフンを使ってください。予約語は使えません。")
		}
		taken, err := q.OrganizationSlugTaken(slug, now.UnixMilli())
		if err != nil {
			return err
		}
		if taken {
			return fail(409, "そのユーザースラッグは使用中か予約済みです。別の値を入力してください。")
		}
		if err = createAccount(q, row.UserID, slug, data.Email, data.Name, now.UnixMilli()); err != nil {
			return err
		}
		if err = q.PutIdentity(row.UserID, provider, identity); err != nil {
			return err
		}
		completed, err = completeOAuth(q, row.UserID, data.IdentitySessionData, login, loginData)
		return err
	})
	if errors.Is(err, errLoopbackExpired) {
		s.setSignupCookie(w, "", -1)
		return loopbackExpired(w)
	}
	var problem apiError
	if errors.As(err, &problem) && (problem.status == 400 || problem.status == 409) {
		return signupPage(w, problem.status, id, slug, problem.Error(), loopbackOrigin)
	}
	if err != nil {
		return err
	}
	s.setSignupCookie(w, "", -1)
	return s.finishOAuth(w, r, completed, data.ReturnPath)
}

func createAccount(q *store.Queries, uid, slug, email, name string, created int64) error {
	org := store.OrganizationRow{ID: auth.Token()[:20], Slug: slug, Data: store.OrganizationData{Name: "Personal projects", Created: created}}
	general := store.ProjectRow{ID: auth.Token()[:20], OrgID: org.ID, Slug: store.GeneralProjectSlug, Created: created, Data: store.ProjectData{Name: "General"}}
	if err := q.PutOrganization(org); err != nil {
		return err
	}
	if err := q.PutOrganizationOwner(org.ID, uid); err != nil {
		return err
	}
	if err := q.PutProject(general); err != nil {
		return err
	}
	if err := q.PutProjectMember(general.ID, uid, "owner"); err != nil {
		return err
	}
	return q.PutUser(store.UserRow{ID: uid, Email: email, Data: store.UserData{Name: name}, PersonalOrgID: &org.ID, GeneralProjectID: &general.ID})
}
