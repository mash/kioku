package server

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

const shell = `<!doctype html><html lang="ja"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}} · kioku</title><style>body{font:16px/1.7 system-ui;max-width:1050px;margin:40px auto;padding:0 24px;background:#fafaf8;color:#202b27}a{color:#176b51}header{border-bottom:1px solid #ddd;margin-bottom:28px}h1{font-size:30px}h2{margin-top:32px}article,pre{background:white;border:1px solid #ddd;border-radius:8px;padding:20px}pre{white-space:pre-wrap;overflow-wrap:anywhere;font:14px/1.7 ui-monospace,monospace}small{color:#666}table{width:100%;border-collapse:collapse}td,th{text-align:left;border-bottom:1px solid #ddd;padding:12px 8px}code{overflow-wrap:anywhere}.badge{background:#e3f0e9;padding:3px 8px;border-radius:6px}</style><header><a href="/">kioku</a> / Shared project context</header>{{template "body" .Data}}</html>`
const messageTemplate = `{{define "body"}}<h1>{{.Message}}</h1>{{end}}`

func render(w http.ResponseWriter, title string, data any, body string) error {
	t, err := template.New("page").Parse(shell + body)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return t.Execute(w, struct {
		Title string
		Data  any
	}{title, data})
}

const homeTemplate = `{{define "body"}}<h1>Shared context, grounded in sources.</h1>{{if .LoggedIn}}<p>プロジェクトを選ぶと、原資料と生成Wikiを閲覧できます。</p>{{range .Projects}}<article><a href="/{{.Namespace}}">{{.Name}}</a> <small>{{.ID}}</small></article>{{else}}<p>まだプロジェクトがありません。保存先のディレクトリで <code>kioku create &lt;name&gt;</code> を実行してください。</p>{{end}}{{else}}<p>ログインして、チームのWikiと原資料を閲覧します。</p>{{range .Providers}}<p><a href="/auth/{{.}}/start{{if $.Return}}?return={{$.Return}}{{end}}">{{.}} でログイン</a></p>{{else}}<p>OAuthの設定がありません。サーバーのGoogleまたはGitHub OAuth設定を確認してください。</p>{{end}}{{end}}{{end}}`

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
	return render(w, "Projects", data, homeTemplate)
}

const projectTemplate = `{{define "body"}}<h1>{{.Project.Name}}</h1><p><span class="badge">{{len .Sources}} sources</span> <small>{{if .Project.WikiIncomplete}}Updating wiki{{else if .Project.WikiUpdatedAt.IsZero}}Wiki generation pending{{else}}Wiki updated {{.Project.WikiUpdatedAt.Format "2006-01-02 15:04 UTC"}}{{end}}</small></p><h2>Wiki を生成</h2><p>サーバーに設定された生成処理を使用します。</p><form method="post" action="/api/projects/{{.Project.ID}}/generate"><label>セッション予算（USD） <input name="budget_usd" type="number" min="0.50" max="{{.MaxBudget}}" step="0.01" value="0.50" required></label> <label><input type="checkbox" name="full" value="true">全資料から再生成</label> <button>生成する</button></form>{{range .RecentJobs}}<p><a href="/api/projects/{{.ProjectID}}/jobs/{{.ID}}">生成ジョブ</a>: {{.Status}}{{if .Error}} — {{.Error}}{{end}}</p>{{end}}<h2>プロジェクトマップ</h2><form method="get"><input name="q" value="{{.Query}}" placeholder="ページを検索"><button>検索</button></form><div style="overflow:auto">{{.Graph}}</div><h2>原資料</h2><p>取得時点の抽出テキストです。原本は保存せず、出典URLを保持します。</p><table><tr><th>資料</th><th>取得日時</th><th>原本</th></tr>{{range .Sources}}<tr><td><a href="/api/projects/{{.ProjectID}}/sources/{{.ID}}/text">{{.Title}}</a><br><small>{{.Kind}} · {{.CreatedBy}}</small></td><td>{{.Created.Format "2006-01-02 15:04 UTC"}}</td><td>{{if .URL}}<a href="{{.URL}}" rel="noreferrer">原本を開く</a>{{else}}URL未登録{{end}}</td></tr>{{end}}</table><h2>Wiki / Maps</h2>{{range .Files}}<p><a href="?file={{.}}">{{.}}</a></p>{{end}}{{if .Body}}<h2>{{.File}}</h2><article>{{.Markdown}}</article><details><summary>原文を表示</summary><pre>{{.Body}}</pre></details>{{end}}{{end}}`

func (s *Server) projectPage(w http.ResponseWriter, r *http.Request, uid string) error {
	data := struct {
		Project         model.Project
		Sources         []model.Source
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

		for _, src := range st.Sources {
			if src.ProjectID == p.ID {
				data.Sources = append(data.Sources, src)
			}
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
		if key == "" && data.File != "wiki/inbox.md" && r.URL.Query().Get("file") != "" {
			return fail(404, "file not found")
		}
		return nil
	})
	if err != nil {
		return err
	}
	intakeFiles := map[string]string{}
	if err = s.addIntakeView(r.Context(), uid, data.Project.ID, intakeFiles); err != nil {
		return err
	}
	if inbox, ok := intakeFiles["wiki/inbox.md"]; ok {
		data.Files = append(data.Files, "wiki/inbox.md")
		if data.File == "wiki/inbox.md" {
			data.Body = inbox
		}
	}
	sort.Strings(data.Files)
	sort.Slice(data.Sources, func(i, j int) bool { return data.Sources[i].Created.Before(data.Sources[j].Created) })
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
	return render(w, data.Project.Name, data, projectTemplate)
}

const inviteTemplate = `{{define "body"}}<h1>プロジェクトへの招待</h1><p>参加すると、このプロジェクトの原資料とWikiを閲覧・取得・更新できます。</p><p>CLIで実行してください。</p><pre>kioku join {{.URL}}</pre>{{end}}`

func (s *Server) invitePage(w http.ResponseWriter, r *http.Request, _ string) error {
	return render(w, "Invitation", struct{ URL string }{s.Config.BaseURL + "/invite/" + r.PathValue("invite")}, inviteTemplate)
}
