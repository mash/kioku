package server

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/internal/wiki"
)

const shell = `<!doctype html><html lang="ja"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}} · kioku</title><style>body{font:16px/1.7 system-ui;max-width:1050px;margin:40px auto;padding:0 24px;background:#fafaf8;color:#202b27}a{color:#176b51}header{display:flex;align-items:center;justify-content:space-between;gap:16px;flex-wrap:wrap;border-bottom:1px solid #ddd;margin-bottom:28px;padding-bottom:16px}header nav{display:flex;align-items:center;gap:16px;flex-wrap:wrap}.login{border:1px solid #176b51;border-radius:6px;padding:5px 14px;text-decoration:none}a:focus-visible{outline:3px solid #176b51;outline-offset:4px}.not-found{text-align:center;padding:48px 0}.error-code{font-size:64px;font-weight:700;margin:0}h1{font-size:30px}h2{margin-top:32px}article,pre{background:white;border:1px solid #ddd;border-radius:8px;padding:20px}pre{white-space:pre-wrap;overflow-wrap:anywhere;font:14px/1.7 ui-monospace,monospace}small{color:#666}table{width:100%;border-collapse:collapse}td,th{text-align:left;border-bottom:1px solid #ddd;padding:12px 8px}code{overflow-wrap:anywhere}.badge{background:#e3f0e9;padding:3px 8px;border-radius:6px}</style><header><div><a href="/">kioku</a> <small>Shared project context</small></div>{{with .Header}}<nav aria-label="アカウント">{{if .LoggedIn}}<a href="/">プロジェクト一覧</a><small>ログイン済み</small>{{else}}<a class="login" href="{{.LoginURL}}">ログイン</a>{{end}}</nav>{{end}}</header><main>{{template "body" .Data}}</main></html>`
const messageTemplate = `{{define "body"}}<h1>{{.Message}}</h1>{{end}}`

type pageHeader struct {
	LoggedIn bool
	LoginURL string
}

func headerFor(r *http.Request, uid string) pageHeader {
	returnPath := r.URL.RequestURI()
	if r.URL.Path == "/" {
		returnPath = r.URL.Query().Get("return")
	}
	target, err := authorizeReturn(returnPath)
	if err != nil {
		target = "/"
	}
	return pageHeader{LoggedIn: uid != "", LoginURL: "/?return=" + url.QueryEscape(target)}
}

func render(w http.ResponseWriter, title string, data any, body string, headers ...pageHeader) error {
	return renderStatus(w, 0, title, data, body, headers...)
}

func renderStatus(w http.ResponseWriter, status int, title string, data any, body string, headers ...pageHeader) error {
	t, err := template.New("page").Parse(shell + body)
	if err != nil {
		return err
	}
	var header *pageHeader
	if len(headers) > 0 {
		header = &headers[0]
	}
	// Finish rendering before committing the status, so template failures remain 500s.
	var output bytes.Buffer
	if err := t.Execute(&output, struct {
		Title  string
		Data   any
		Header *pageHeader
	}{title, data, header}); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if status != 0 {
		w.WriteHeader(status)
	}
	_, err = w.Write(output.Bytes())
	return err
}

const notFoundTemplate = `{{define "body"}}<section class="not-found"><p class="error-code">404</p><h1>ページが見つかりません</h1>{{if not .LoggedIn}}<p>アクセスできるページか確認するには、ログインしてください。</p>{{end}}</section>{{end}}`

// Keep authentication and agent authorization in wrap; only browser page errors
// receive HTML. API error statuses and bodies are unaffected.
func (s *Server) web(fn handler, required bool) http.HandlerFunc {
	return s.wrap(func(w http.ResponseWriter, r *http.Request, uid string) error {
		var err error
		if required && uid == "" {
			err = fail(404, "not found")
		} else {
			err = fn(w, r, uid)
		}
		var problem apiError
		if errors.As(err, &problem) && problem.status == http.StatusNotFound {
			header := headerFor(r, uid)
			if r.Pattern == "GET /{org}/{slug}/views/{view}" {
				header.LoginURL = "/"
			}
			return renderStatus(w, http.StatusNotFound, "ページが見つかりません", header, notFoundTemplate, header)
		}
		return err
	}, false)
}

const homeTemplate = `{{define "body"}}<h1>Shared context, grounded in sources.</h1>{{if .LoggedIn}}<p>プロジェクトを選ぶと、原資料と生成Wikiを閲覧できます。</p>{{range .Projects}}<article><a href="/{{.Namespace}}">{{.Name}}</a> <small>{{.Namespace}}</small></article>{{else}}<p>まだプロジェクトがありません。保存先のディレクトリで <code>kioku project create &lt;name&gt;</code> を実行してください。</p>{{end}}{{else}}<p>ログインして、チームのWikiと原資料を閲覧します。</p>{{range .Providers}}<p><a href="/auth/{{.}}/start{{if $.Return}}?return={{$.Return}}{{end}}">{{.}} でログイン</a></p>{{else}}<p>OAuthの設定がありません。サーバーのGoogleまたはGitHub OAuth設定を確認してください。</p>{{end}}{{end}}{{end}}`

func (s *Server) home(w http.ResponseWriter, r *http.Request, uid string) error {
	if r.URL.Path != "/" {
		return fail(404, "not found")
	}
	returnPath, err := authorizeReturn(r.URL.Query().Get("return"))
	if err != nil {
		return err
	}
	data := struct {
		Return    string
		LoggedIn  bool
		Projects  []model.Project
		Providers []string
	}{Return: returnPath, LoggedIn: uid != ""}
	for name, p := range s.Config.Providers {
		if p.ClientID != "" {
			data.Providers = append(data.Providers, name)
		}
	}
	sort.Strings(data.Providers)
	if err := s.Store.Read(r.Context(), func(q *store.Queries) error {
		rows, err := q.ProjectsForUser(uid)
		if err != nil {
			return err
		}
		for _, row := range rows {
			p, err := projectProjection(q, row)
			if err != nil {
				return err
			}
			data.Projects = append(data.Projects, p)
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(data.Projects, func(i, j int) bool { return data.Projects[i].Name < data.Projects[j].Name })
	return render(w, "Projects", data, homeTemplate, headerFor(r, uid))
}

const projectTemplate = `{{define "body"}}
<style>
.project-columns{display:grid;grid-template-columns:minmax(0,1fr) minmax(15rem,26%);gap:2rem;align-items:start}
.views-sidebar{border-left:1px solid #ddd;padding-left:1.25rem;overflow-wrap:anywhere}
.views-sidebar h2{margin:0}.views-sidebar ul{padding-left:1.2rem}.views-sidebar li{margin-bottom:.5rem}
.view-settings label{display:block;margin:.6rem 0}.view-settings input,.view-settings textarea{width:100%;box-sizing:border-box;font:inherit}
.view-settings textarea{min-height:8rem}.view-settings summary{cursor:pointer}
@media(max-width:710px){.project-columns{grid-template-columns:minmax(0,1fr)}.views-sidebar{border-left:0;border-top:1px solid #ddd;padding:1rem 0}}
</style>
<h1>{{.Project.Name}}</h1><p><small>{{if .Project.WikiIncomplete}}Updating wiki{{else if .Project.WikiUpdatedAt.IsZero}}Wiki generation pending{{else}}Wiki updated {{.Project.WikiUpdatedAt.Format "2006-01-02 15:04 UTC"}}{{end}}</small></p>
<form method="post" action="/api/projects/{{.Project.Namespace}}/generate"><label>セッション予算（USD） <input name="budget_usd" type="number" min="0.50" max="{{.MaxBudget}}" step="0.01" value="0.50" required></label> <button>生成する</button></form>
{{range .RecentJobs}}<p><a href="/api/projects/{{$.Project.Namespace}}/jobs/{{.ID}}">生成ジョブ</a>: {{.Status}}{{if .Error}} — {{.Error}}{{end}}</p>{{end}}
<div class="project-columns">
  <section aria-label="Wiki">
    {{if .Body}}<article>{{.Markdown}}</article><details><summary>原文を表示</summary><pre>{{.Body}}</pre></details>{{end}}
    <h2>Wiki ページ</h2>{{range .Files}}<p><a href="?file={{.}}">{{.}}</a></p>{{end}}
  </section>
  <aside class="views-sidebar" aria-labelledby="views-title">
    <h2 id="views-title">Views</h2>
    {{if .CanEdit}}
    <details class="view-settings">
      <summary aria-label="View を追加">＋</summary>
      <form method="post" action="/api/projects/{{.Project.Namespace}}/views" data-view-action="POST">
        <label>タイトル <input name="title" required></label>
        <label>プロンプト <textarea name="prompt" required></textarea></label>
        <button type="button" data-preset-title="" data-preset-prompt="">自由入力</button>
        {{range .Presets}}<button type="button" data-preset-title="{{.Title}}" data-preset-prompt="{{.Prompt}}">{{.Label}}</button> {{end}}
        <p><button>保存</button></p>
        <p role="alert"></p>
      </form>
    </details>
    {{end}}
    <ul>{{range .Views}}<li><a href="/{{$.Project.Namespace}}/views/{{.Slug}}">{{.Title}}</a><br><small>{{.Status}}{{if .GeneratedAt}} · {{.GeneratedAt.Format "2006-01-02 15:04 UTC"}}{{end}}{{if .Error}} · {{.Error}}{{end}}</small></li>{{else}}<li>View はまだありません</li>{{end}}</ul>
  </aside>
</div>
{{if .CanEdit}}` + viewSettingsScriptTag + `{{end}}
{{end}}`

func (s *Server) projectPage(w http.ResponseWriter, r *http.Request, uid string) error {
	data := struct {
		Project    model.Project
		Files      []string
		File, Body string
		Markdown   template.HTML
		MaxBudget  string
		RecentJobs []model.Job
		CanEdit    bool
		Views      []viewPageItem
		Presets    []viewPreset
	}{}
	unlock, err := s.lockProject(r.Context(), r.PathValue("project"))
	if err != nil {
		return err
	}
	defer unlock()
	var key blob.Key
	var rev model.Revision
	err = s.Store.Read(r.Context(), func(q *store.Queries) error {
		p, err := member(q, r.PathValue("project"), uid)
		if err != nil {
			return err
		}
		data.Project = p
		data.CanEdit = p.Members[uid] == "owner"
		data.Presets = projectViewPresets
		data.MaxBudget = fmt.Sprintf("%.2f", float64(s.Config.AgentMaxSessionCents)/100)
		jobs, err := q.RecentGenerationJobs(p.ID, 5)
		if err != nil {
			return err
		}
		for _, row := range jobs {
			j, err := jobProjection(q, row)
			if err != nil {
				return err
			}
			j.Error = strings.ReplaceAll(j.Error, p.ID, "[project]")
			data.RecentJobs = append(data.RecentJobs, j)
		}
		project, err := q.Project(p.ID)
		if err != nil {
			return err
		}
		rev, err = wikiProjection(q, project)
		if err != nil {
			return err
		}
		for name := range rev.Files {
			if wiki.ValidFile(name) {
				data.Files = append(data.Files, name)
			}
		}
		data.File = r.URL.Query().Get("file")
		if data.File == "" {
			data.File = "wiki/index.md"
		}
		if !wiki.ValidFile(data.File) {
			return fail(404, "file not found")
		}
		key = rev.Files[data.File]
		if key == "" && r.URL.Query().Get("file") != "" {
			return fail(404, "file not found")
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(data.Files)
	if key != "" {
		body, err := s.wikiFile(r.Context(), rev, data.File)
		if err != nil {
			return err
		}
		data.Body = body
	}
	data.Markdown = markdownView(data.Body, data.File, data.Files)
	views, err := s.listViewsLocked(r.Context(), data.Project.ID)
	if err != nil {
		return err
	}
	for _, view := range views {
		data.Views = append(data.Views, makeViewPageItem(view))
	}
	if data.CanEdit {
		w.Header().Set("Content-Security-Policy", pageCSP+viewSettingsCSP)
	}
	w.Header().Set("Referrer-Policy", "same-origin")
	return render(w, data.Project.Name, data, projectTemplate, headerFor(r, uid))
}

const inviteTemplate = `{{define "body"}}<h1>プロジェクトへの招待</h1><p>参加すると、このプロジェクトの原資料とWikiを閲覧・取得・更新できます。</p><p>CLIで実行してください。</p><pre>kioku project join {{.URL}}</pre>{{end}}`

func (s *Server) invitePage(w http.ResponseWriter, r *http.Request, uid string) error {
	return render(w, "Invitation", struct{ URL string }{s.Config.BaseURL + "/invite/" + r.PathValue("invite")}, inviteTemplate, headerFor(r, uid))
}
