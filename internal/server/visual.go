package server

import (
	"bytes"
	"html/template"
	"net/url"
	"path"
	"strings"

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
	// Only allow sanitized HTML.
	return template.HTML(bluemonday.UGCPolicy().Sanitize(b.String()))
}
