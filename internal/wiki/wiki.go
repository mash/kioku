package wiki

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mash/kioku/internal/model"
)

type Input struct {
	Source model.Source `json:"source"`
	Text   string       `json:"text"`
}
type Node struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
}
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

var pagePath = regexp.MustCompile(`^wiki/[A-Za-z0-9][A-Za-z0-9_/-]*\.md$`)

// MaxSnapshotBytes includes agent pages, source pages, provenance, and maps.
const MaxSnapshotBytes = 64 << 20

func ValidFile(p string) bool {
	return p != "" && len(p) < 160 && path.Clean(p) == p && (pagePath.MatchString(p) || p == "maps/graph.json" || p == "maps/graph.mmd")
}
func title(s string) string { return strings.Join(strings.Fields(s), " ") }

// Assemble adds service-owned evidence pages, provenance and link maps.
func Assemble(inputs []Input, pages map[string]string) (map[string]string, error) {
	files := map[string]string{}
	total := 0
	add := func(name string, pieces ...string) error {
		n := total - len(files[name])
		for _, part := range pieces {
			n += len(part)
		}
		if n > MaxSnapshotBytes {
			return &SnapshotTooLargeError{Bytes: int64(n)}
		}
		files[name] = strings.Join(pieces, "")
		total = n
		return nil
	}
	var index strings.Builder
	index.WriteString("# Project wiki\n\nGenerated from shared project sources.\n\n")
	for _, in := range inputs {
		file := "wiki/source-" + in.Source.ID + ".md"
		if err := add(file, "# ", title(in.Source.Title), "\n\n", in.Text, "\n\n## Source\n\n", sourceLink(in.Source), "\n"); err != nil {
			return nil, err
		}
		fmt.Fprintf(&index, "- [%s](source-%s.md)\n", title(in.Source.Title), in.Source.ID)
	}
	if err := add("wiki/index.md", index.String()); err != nil {
		return nil, err
	}
	provenance := sources(inputs)
	for name, body := range pages {
		if err := add(name, StripSnapshots(body), "\n\n## Source snapshots\n\n", provenance); err != nil {
			return nil, err
		}
	}
	graph := Graph{Nodes: []Node{}, Edges: []Edge{}}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	seenEdges := map[string]bool{}
	for _, name := range names {
		graph.Nodes = append(graph.Nodes, Node{ID: name, Title: pageTitle(files[name], name), Kind: "page"})
		// Scan one match at a time; a page full of duplicate links must not
		// allocate a match slice proportional to its input length.
		remaining := files[name]
		for len(graph.Edges) < 10000 {
			m := markdownLink.FindStringSubmatchIndex(remaining)
			if m == nil {
				break
			}
			target, _, _ := strings.Cut(remaining[m[2]:m[3]], "#")
			remaining = remaining[m[1]:]
			if strings.Contains(target, ":") || strings.HasPrefix(target, "/") {
				continue
			}
			target = path.Clean(path.Join(path.Dir(name), target))
			if _, ok := files[target]; ok {
				key := name + "\x00" + target
				if !seenEdges[key] && len(graph.Edges) < 10000 {
					seenEdges[key] = true
					graph.Edges = append(graph.Edges, Edge{From: name, To: target, Kind: "links-to"})
				}
			}
		}
	}
	b, _ := json.MarshalIndent(graph, "", "  ")
	if err := add("maps/graph.json", string(b)); err != nil {
		return nil, err
	}
	var mermaid strings.Builder
	mermaid.WriteString("graph TD\n")
	ids := map[string]string{}
	for i, n := range graph.Nodes {
		id := fmt.Sprintf("n%d", i)
		ids[n.ID] = id
		fmt.Fprintf(&mermaid, "  %s[\"%s\"]\n", id, mermaidText(n.Title))
	}
	for _, e := range graph.Edges {
		fmt.Fprintf(&mermaid, "  %s --> %s\n", ids[e.From], ids[e.To])
	}
	if err := add("maps/graph.mmd", mermaid.String()); err != nil {
		return nil, err
	}
	return files, nil
}

var markdownLink = regexp.MustCompile(`\[[^\]]*\]\(([^) ]+)\)`)

func mermaidText(s string) string {
	r := strings.NewReplacer("&", "&amp;", "\"", "&quot;", "<", "&lt;", ">", "&gt;", "\n", " ", "[", "(", "]", ")")
	return r.Replace(s)
}
func pageTitle(body, fallback string) string {
	for body != "" {
		line, rest, _ := strings.Cut(body, "\n")
		body = rest
		if strings.HasPrefix(line, "# ") {
			// Graph labels are summaries. Keep the complete page text above,
			// but bound labels before JSON/HTML escaping can amplify them.
			label := strings.TrimPrefix(line, "# ")
			runes := 0
			for i := range label {
				if runes == 256 {
					return label[:i]
				}
				runes++
			}
			return label
		}
	}
	return fallback
}
func sourceLink(s model.Source) string {
	return fmt.Sprintf("- [%s](/api/projects/%s/sources/%s/text) — snapshot `%s`, contributor `%s`, %s", title(s.Title), s.ProjectID, s.ID, s.ContentHash, s.CreatedBy, s.Created.UTC().Format(time.RFC3339))
}
func sources(inputs []Input) string {
	var b strings.Builder
	for _, in := range inputs {
		b.WriteString(sourceLink(in.Source))
		b.WriteByte('\n')
	}
	return b.String()
}

// StripSnapshots removes the service-owned trailing provenance section.
func StripSnapshots(body string) string {
	if i := strings.LastIndex(body, "\n\n## Source snapshots\n\n"); i >= 0 {
		return body[:i]
	}
	return body
}
