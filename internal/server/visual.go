package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/url"
	"path"
	"strings"

	"github.com/mash/kioku/internal/wiki"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"golang.org/x/net/html"
)

func markdownView(body, file string, files []string) template.HTML {
	var b bytes.Buffer
	if err := goldmark.Convert([]byte(body), &b); err != nil {
		return ""
	}
	safe := bluemonday.UGCPolicy().SanitizeBytes(b.Bytes())
	root, err := html.Parse(bytes.NewReader(safe))
	if err != nil {
		return ""
	}
	known := map[string]bool{}
	for _, f := range files {
		known[f] = true
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for i, a := range n.Attr {
				if a.Key == "href" {
					u, err := url.Parse(a.Val)
					if err != nil {
						continue
					}
					if u.Scheme == "" && u.Host == "" && !strings.HasPrefix(u.Path, "/") && u.Path != "" {
						target := path.Clean(path.Join(path.Dir(file), u.Path))
						if known[target] {
							n.Attr[i].Val = "?file=" + url.QueryEscape(target)
							if u.Fragment != "" {
								n.Attr[i].Val += "#" + url.PathEscape(u.Fragment)
							}
						}
					}
				}
			}
			n.Attr = append(n.Attr, html.Attribute{Key: "rel", Val: "noopener noreferrer"})
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	b.Reset()
	_ = html.Render(&b, root)
	// Only allow sanitized HTML; graph SVG is generated separately by templates.
	return template.HTML(bluemonday.UGCPolicy().Sanitize(b.String()))
}

const graphTemplate = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1000 {{.Height}}" role="img" aria-label="Project map"><defs><marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="5" markerHeight="5" orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z" fill="#709487"/></marker></defs>{{range .Edges}}<path d="M {{.X1}} {{.Y1}} L {{.X2}} {{.Y2}}" stroke="#9ab7ab" stroke-width="1.5" marker-end="url(#arrow)"/>{{end}}{{range .Nodes}}<a href="?file={{.ID}}"><rect x="{{.X}}" y="{{.Y}}" width="280" height="60" rx="8" fill="#e3f0e9" stroke="#176b51"/><text x="{{.TX}}" y="{{.TY}}" font-size="13" fill="#163e30">{{.Title}}</text><title>{{.Full}}</title></a>{{end}}</svg>`

func graphView(raw, query string) template.HTML {
	var graph wiki.Graph
	if json.Unmarshal([]byte(raw), &graph) != nil {
		return ""
	}
	type node struct {
		ID, Title, Full string
		X, Y, TX, TY    int
	}
	type edge struct{ X1, Y1, X2, Y2 int }
	data := struct {
		Height int
		Nodes  []node
		Edges  []edge
	}{Height: 100}
	positions := map[string]node{}
	for _, n := range graph.Nodes {
		if query != "" && !strings.Contains(strings.ToLower(n.Title+" "+n.ID), strings.ToLower(query)) {
			continue
		}
		i := len(data.Nodes)
		x, y := 25+(i%3)*325, 25+(i/3)*115
		r := []rune(n.Title)
		title := n.Title
		if len(r) > 30 {
			title = string(r[:29]) + "…"
		}
		v := node{ID: n.ID, Title: title, Full: n.Title, X: x, Y: y, TX: x + 12, TY: y + 35}
		positions[n.ID] = v
		data.Nodes = append(data.Nodes, v)
		data.Height = y + 100
	}
	for _, e := range graph.Edges {
		a, ok := positions[e.From]
		b, ok2 := positions[e.To]
		if ok && ok2 {
			data.Edges = append(data.Edges, edge{a.X + 140, a.Y + 60, b.X + 140, b.Y})
		}
	}
	t, err := template.New("graph").Parse(graphTemplate)
	if err != nil {
		return ""
	}
	var out bytes.Buffer
	if err = t.Execute(&out, data); err != nil {
		return template.HTML(fmt.Sprintf("<p>Map unavailable</p>"))
	}
	return template.HTML(out.String())
}
