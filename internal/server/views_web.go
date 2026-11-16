package server

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

// The Kioku settings script runs in the parent page. Generated content is
// rendered in a sandboxed frame with its own stricter policy.
const viewSettingsScript = `
for (const settings of document.querySelectorAll('details[data-view-settings]')) {
  settings.addEventListener('toggle', async () => {
    if (!settings.open || settings.dataset.loaded) return;
    const form = settings.querySelector('form[data-view-action="PATCH"]');
    const fields = form.querySelector('fieldset');
    const status = form.querySelector('[role="alert"]');
    try {
      const response = await fetch(form.action);
      const value = await response.json();
      if (!response.ok) {
        status.textContent = value.error || 'View の設定を取得できませんでした';
        return;
      }
      form.elements.namedItem('title').value = value.title;
      form.elements.namedItem('prompt').value = value.prompt;
      fields.disabled = false;
      status.textContent = '';
      settings.dataset.loaded = 'true';
    } catch {
      status.textContent = 'View の設定を取得できませんでした';
    }
  });
}
for (const form of document.querySelectorAll('form[data-view-action]')) {
  for (const button of form.querySelectorAll('[data-preset-title]')) {
    button.addEventListener('click', () => {
      form.elements.namedItem('title').value = button.dataset.presetTitle;
      form.elements.namedItem('prompt').value = button.dataset.presetPrompt;
    });
  }
  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    const status = form.querySelector('[role="alert"]');
    const method = form.dataset.viewAction;
    const button = event.submitter;
    if (button) button.disabled = true;
    status.textContent = '';
    try {
      const options = {method};
      if (method !== 'DELETE') {
        options.headers = {'Content-Type': 'application/json'};
        options.body = JSON.stringify(Object.fromEntries(new FormData(form)));
      }
      const response = await fetch(form.action, options);
      if (!response.ok) {
        const error = await response.json();
        status.textContent = error.error || 'View を保存できませんでした';
        return;
      }
      window.location.assign(form.dataset.success || window.location.pathname);
    } catch {
      status.textContent = 'View の操作に失敗しました';
    } finally {
      if (button) button.disabled = false;
    }
  });
}
`

const viewSettingsScriptTag = "<script>" + viewSettingsScript + "</script>"

var viewSettingsCSP = func() string {
	digest := sha256.Sum256([]byte(viewSettingsScript))
	return "; connect-src 'self'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "'"
}()

type viewPageItem struct {
	Slug, Title, Status, Error string
	GeneratedAt                *time.Time
}

var viewStatusLabels = map[string]string{
	"pending": "未生成", "running": "生成中", "completed": "生成済み",
	"failed": "生成失敗", "skipped": "生成をスキップ",
}

func makeViewPageItem(v viewDefinition) viewPageItem {
	return viewPageItem{v.Slug, v.Title, viewStatusLabels[v.Status], v.Error, v.GeneratedAt}
}

const viewTemplate = `{{define "body"}}
<style>
iframe.view-content{width:100%;height:70vh;border:1px solid #ddd;border-radius:8px}
.view-settings textarea{display:block;width:100%;min-height:8rem}
.view-settings form{margin:.75rem 0}.view-settings summary{cursor:pointer}
</style>
<p><a href="/{{.Namespace}}">Wiki</a></p>
<h1>{{.View.Title}}</h1>
<p>{{.View.Status}}{{if .View.Error}} · {{.View.Error}}{{end}}</p>
{{if .IsWiki}}
  {{if .View.GeneratedAt}}<p><small>最終更新 {{.View.GeneratedAt.Format "2006-01-02 15:04 UTC"}}</small></p>{{end}}
{{else if .Document}}
  <p><small>最終成功 {{.View.GeneratedAt.Format "2006-01-02 15:04 UTC"}} · {{if .WikiUpdated}}生成後に Wiki が更新されました{{else}}生成後の Wiki 更新なし{{end}}</small></p>
  <iframe class="view-content" title="{{.View.Title}}" sandbox srcdoc="{{.Document}}"></iframe>
  {{if .Citations}}<h2>Wiki の出典</h2><ul>{{range .Citations}}<li><a href="/{{$.Namespace}}?file={{.Path}}">{{.Label}}</a></li>{{end}}</ul>{{end}}
{{else if not .Pending}}<p>未生成</p>{{end}}
{{if .CanEdit}}
<details class="view-settings" data-view-settings>
  <summary>View の設定</summary>
  <form method="post" action="{{.API}}" data-view-action="PATCH">
    <fieldset disabled>
      <label>タイトル <input name="title" required></label>
      <label>プロンプト <textarea name="prompt" required></textarea></label>
      <button>保存</button>
    </fieldset>
    <p role="alert"></p>
  </form>
  {{if not .IsWiki}}
  <form method="post" action="{{.API}}" data-view-action="DELETE" data-success="/{{.Namespace}}">
    <button>View を削除</button>
    <p role="alert"></p>
  </form>
  {{end}}
</details>
` + viewSettingsScriptTag + `
{{end}}
{{end}}`

func (s *Server) viewPage(w http.ResponseWriter, r *http.Request, uid string) error {
	ctx := r.Context()
	resolve := func() (p model.Project, rev wikiTree, err error) {
		err = s.Store.Read(ctx, func(q *store.Queries) error {
			var err error
			p, err = namespaceProject(q, r.PathValue("org"), r.PathValue("slug"), uid)
			if err != nil {
				return err
			}
			row, err := q.Project(p.ID)
			if err != nil {
				return err
			}
			rev, err = wikiProjection(q, row)
			return err
		})
		return p, rev, err
	}
	p, _, err := resolve()
	if err != nil {
		return err
	}
	unlock, err := s.storage.lock(ctx, p.ID)
	if err != nil {
		return err
	}
	defer unlock()
	current, rev, err := resolve()
	if err != nil {
		return err
	}
	if current.ID != p.ID {
		return fail(404, "project not found")
	}
	v, err := s.viewBySlugLocked(ctx, current.ID, r.PathValue("view"))
	if err != nil {
		return err
	}
	isWiki := v.Slug == wikiViewSlug
	data := struct {
		Namespace, API, Document              string
		View                                  viewPageItem
		CanEdit, WikiUpdated, Pending, IsWiki bool
		Citations                             []viewCitation
	}{
		Namespace: p.Namespace, API: "/api/projects/" + p.Namespace + "/views/" + v.Slug,
		View: makeViewPageItem(v), CanEdit: current.Members[uid] == "owner",
		Pending: v.Status == "pending", IsWiki: isWiki,
	}
	if !isWiki {
		body, err := s.readViewBundleLocked(ctx, p.ID, v)
		if err != nil && !isNotFoundView(err) {
			return err
		}
		if err == nil {
			bundle, err := readViewBundle(body)
			if err != nil {
				return err
			}
			data.Document = bundle.Document()
			if updated := viewHasNewWiki(v, current); updated != nil {
				data.WikiUpdated = *updated
			}
			for _, citation := range bundle.Citations() {
				if _, exists := rev.Files[citation.Path]; exists {
					data.Citations = append(data.Citations, citation)
				}
			}
		}
	}
	policy := pageCSP + "; img-src data:"
	if data.CanEdit {
		policy += viewSettingsCSP
		w.Header().Set("Referrer-Policy", "same-origin")
	}
	w.Header().Set("Content-Security-Policy", policy)
	return render(w, v.Title, data, viewTemplate, headerFor(r, uid))
}
