package server

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
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
			return renderStatus(w, http.StatusNotFound, "ページが見つかりません", header, notFoundTemplate, header)
		}
		return err
	}, false)
}

const homeTemplate = `{{define "body"}}<h1>Shared context, grounded in sources.</h1>{{if .LoggedIn}}<p>プロジェクトを選ぶと、原資料と生成Wikiを閲覧できます。</p>{{range .Projects}}<article><a href="/{{.Namespace}}">{{.Name}}</a> <small>{{.ID}}</small></article>{{else}}<p>まだプロジェクトがありません。保存先のディレクトリで <code>kioku project create &lt;name&gt;</code> を実行してください。</p>{{end}}{{else}}<p>ログインして、チームのWikiと原資料を閲覧します。</p>{{range .Providers}}<p><a href="/auth/{{.}}/start{{if $.Return}}?return={{$.Return}}{{end}}">{{.}} でログイン</a></p>{{else}}<p>OAuthの設定がありません。サーバーのGoogleまたはGitHub OAuth設定を確認してください。</p>{{end}}{{end}}{{end}}`

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
	if err := s.DB.View(func(st model.State) error {
		for _, p := range st.Projects {
			if p.Members[uid] != "" {
				data.Projects = append(data.Projects, p)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(data.Projects, func(i, j int) bool { return data.Projects[i].Name < data.Projects[j].Name })
	return render(w, "Projects", data, homeTemplate, headerFor(r, uid))
}

const projectTemplate = `{{define "body"}}<h1>{{.Project.Name}}</h1><p><small>{{if .Project.WikiIncomplete}}Updating wiki{{else if .Project.WikiUpdatedAt.IsZero}}Wiki generation pending{{else}}Wiki updated {{.Project.WikiUpdatedAt.Format "2006-01-02 15:04 UTC"}}{{end}}</small></p><h2>Wiki を生成</h2><p>サーバーに設定された生成処理を使用します。</p><form method="post" action="/api/projects/{{.Project.ID}}/generate"><label>セッション予算（USD） <input name="budget_usd" type="number" min="0.50" max="{{.MaxBudget}}" step="0.01" value="0.50" required></label> <button>生成する</button></form>{{range .RecentJobs}}<p><a href="/api/projects/{{.ProjectID}}/jobs/{{.ID}}">生成ジョブ</a>: {{.Status}}{{if .Error}} — {{.Error}}{{end}}</p>{{end}}<h2>プロジェクトマップ</h2><form method="get"><input name="q" value="{{.Query}}" placeholder="ページを検索"><button>検索</button></form><div style="overflow:auto">{{.Graph}}</div><h2>Wiki / Maps</h2>{{range .Files}}<p><a href="?file={{.}}">{{.}}</a></p>{{end}}{{if .Body}}<h2>{{.File}}</h2><article>{{.Markdown}}</article><details><summary>原文を表示</summary><pre>{{.Body}}</pre></details>{{end}}{{end}}`

func (s *Server) projectPage(w http.ResponseWriter, r *http.Request, uid string) error {
	data := struct {
		Project         model.Project
		Files           []string
		File, Body      string
		Query           string
		Graph, Markdown template.HTML
		MaxBudget       string
		RecentJobs      []model.Job
	}{}
	var key blob.Key
	err := s.DB.View(func(st model.State) error {
		p, err := member(st, r.PathValue("project"), uid)
		if err != nil {
			return err
		}
		data.Project = p
		data.MaxBudget = fmt.Sprintf("%.2f", float64(s.Config.AgentMaxSessionCents)/100)
		for _, j := range st.Jobs {
			if j.ProjectID == p.ID && j.Kind == "generate" {
				data.RecentJobs = append(data.RecentJobs, j)
			}
		}
		sort.Slice(data.RecentJobs, func(i, j int) bool { return data.RecentJobs[i].Created.After(data.RecentJobs[j].Created) })
		if len(data.RecentJobs) > 5 {
			data.RecentJobs = data.RecentJobs[:5]
		}

		rev := st.Revisions[p.Revision]
		for name := range rev.Files {
			data.Files = append(data.Files, name)
		}
		data.File = r.URL.Query().Get("file")
		if data.File == "" {
			data.File = "wiki/index.md"
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
		b, err := s.Blobs.Get(r.Context(), key)
		if err != nil {
			return err
		}
		data.Body = string(b)
	}
	data.Query = r.URL.Query().Get("q")
	data.Markdown = markdownView(data.Body, data.File, data.Files)
	var graphKey blob.Key
	if err = s.DB.View(func(st model.State) error {
		graphKey = st.Revisions[data.Project.Revision].Files["maps/graph.json"]
		return nil
	}); err != nil {
		return err
	}
	if graphKey != "" {
		b, err := s.Blobs.Get(r.Context(), graphKey)
		if err != nil {
			return err
		}
		data.Graph = graphView(string(b), data.Query)
	}
	return render(w, data.Project.Name, data, projectTemplate, headerFor(r, uid))
}

const inviteTemplate = `{{define "body"}}<h1>プロジェクトへの招待</h1><p>参加すると、このプロジェクトの原資料とWikiを閲覧・取得・更新できます。</p><p>CLIで実行してください。</p><pre>kioku project join {{.URL}}</pre>{{end}}`

func (s *Server) invitePage(w http.ResponseWriter, r *http.Request, uid string) error {
	return render(w, "Invitation", struct{ URL string }{s.Config.BaseURL + "/invite/" + r.PathValue("invite")}, inviteTemplate, headerFor(r, uid))
}
