// Package source retrieves external material. URL fetches never receive user
// credentials; provider credentials are sent only to fixed provider API hosts.
package source

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mash/kioku/internal/auth"
	"golang.org/x/net/html"
)

const MaxBytes = 4 << 20

type Document struct {
	Title, URL, Kind, Version, MediaType, Text string
	Original                                   []byte
}
type Fetcher struct {
	Public      *http.Client
	ProviderGet func(context.Context, string, string, any) error
}

func New() *Fetcher { return &Fetcher{Public: SafeClient(), ProviderGet: auth.Get} }
func SafeClient() *http.Client {
	dial := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, DisableKeepAlives: true}
	// Resolve and validate every address, then dial that exact IP to prevent DNS rebinding.
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errors.New("no addresses")
		}
		for _, ip := range ips {
			if !publicIP(ip) {
				return nil, errors.New("private or reserved network address blocked")
			}
		}
		var last error
		for _, ip := range ips {
			c, err := dial.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return ValidateURL(req.URL)
	}}
}
func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, s := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "2001::/32", "2002::/16", "64:ff9b::/96", "64:ff9b:1::/48"} {
		if netip.MustParsePrefix(s).Contains(ip) {
			return false
		}
	}
	return true
}
func ValidateURL(u *url.URL) error {
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return errors.New("only public HTTPS URLs on port 443 are supported")
	}
	h := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") {
		return errors.New("local hostname blocked")
	}
	if ip, err := netip.ParseAddr(h); err == nil && !publicIP(ip) {
		return errors.New("private address blocked")
	}
	return nil
}
func Kind(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "web"
	}
	switch strings.ToLower(u.Hostname()) {
	case "docs.google.com":
		return "google"
	case "github.com":
		return "github"
	}
	return "web"
}
func (f *Fetcher) Fetch(ctx context.Context, raw, token string) (Document, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Document{}, err
	}
	if err = ValidateURL(u); err != nil {
		return Document{}, err
	}
	switch Kind(raw) {
	case "google":
		return f.google(ctx, u, token)
	case "github":
		return f.github(ctx, u, token)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return Document{}, err
	}
	req.Header.Set("User-Agent", "kioku-wiki/0.1")
	req.Header.Set("Accept", "text/html,text/plain,text/markdown")
	resp, err := f.Public.Do(req)
	if err != nil {
		return Document{}, fmt.Errorf("source could not be fetched over HTTP; write the original contents and source URL to a Markdown/text file, then run kioku put --source-url <original-URL> <file>: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Document{}, fmt.Errorf("source returned HTTP %d; write the original contents and source URL to a Markdown/text file, then run kioku put --source-url <original-URL> <file>", resp.StatusCode)
	}
	b, err := read(resp.Body)
	if err != nil {
		return Document{}, err
	}
	if !utf8.Valid(b) || strings.ContainsRune(string(b), 0) {
		return Document{}, errors.New("web source must be UTF-8 text")
	}
	media := strings.ToLower(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	d := Document{Title: path.Base(u.Path), Original: b, Text: string(b), MediaType: media}
	if media == "text/html" {
		d.Title, d.Text = extractHTML(b)
	} else if media != "text/plain" && media != "text/markdown" && media != "application/json" {
		return Document{}, errors.New("unsupported web content type; export as Markdown, text or HTML")
	}
	if strings.TrimSpace(d.Text) == "" {
		return Document{}, errors.New("page has no readable text over HTTP; write the original contents and source URL to a Markdown/text file, then run kioku put --source-url <original-URL> <file>")
	}
	d.URL = raw
	d.Kind = "web"
	if d.Title == "" || d.Title == "." || d.Title == "/" {
		d.Title = u.Hostname()
	}
	return d, nil
}
func File(name, content string) (Document, error) {
	if len(content) > MaxBytes {
		return Document{}, errors.New("source exceeds 4 MiB")
	}
	if !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return Document{}, errors.New("source must be UTF-8 text")
	}
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".md", ".txt", ".html", ".htm", ".json", ".go", ".js", ".ts", ".tsx", ".jsx", ".yaml", ".yml", ".csv", ".css":
	default:
		return Document{}, errors.New("unsupported file extension; use text, Markdown, HTML or code")
	}
	d := Document{Title: path.Base(name), Kind: "file", MediaType: "text/plain", Text: content, Original: []byte(content)}
	if ext == ".html" || ext == ".htm" {
		title, text := extractHTML(d.Original)
		d.Text = text
		d.MediaType = "text/html"
		if title != "" {
			d.Title = title
		}
	}
	if strings.TrimSpace(d.Text) == "" {
		return Document{}, errors.New("source is empty")
	}
	return d, nil
}
func extractHTML(b []byte) (string, string) {
	n, err := html.Parse(strings.NewReader(string(b)))
	if err != nil {
		return "", ""
	}
	var title, out strings.Builder
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, inTitle bool) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "template":
				return
			case "title":
				inTitle = true
			case "p", "div", "section", "article", "br", "li", "h1", "h2", "h3", "tr":
				out.WriteString("\n")
			}
		}
		if n.Type == html.TextNode {
			if inTitle {
				title.WriteString(n.Data)
			} else if s := strings.Join(strings.Fields(n.Data), " "); s != "" {
				out.WriteString(s)
				out.WriteByte(' ')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inTitle)
		}
	}
	walk(n, false)
	return strings.TrimSpace(title.String()), strings.TrimSpace(out.String())
}

var docPath = regexp.MustCompile(`^/document/d/([a-zA-Z0-9_-]+)`)

func (f *Fetcher) google(ctx context.Context, u *url.URL, token string) (Document, error) {
	if token == "" {
		return Document{}, errors.New("run kioku connect google first")
	}
	m := docPath.FindStringSubmatch(u.Path)
	if len(m) < 2 {
		return Document{}, errors.New("expected a Google Docs document URL")
	}
	var raw json.RawMessage
	if err := f.ProviderGet(ctx, "https://docs.googleapis.com/v1/documents/"+m[1]+"?includeTabsContent=true", token, &raw); err != nil {
		return Document{}, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Document{}, err
	}
	var text strings.Builder
	googleText(doc, &text)
	title, _ := doc["title"].(string)
	version, _ := doc["revisionId"].(string)
	if strings.TrimSpace(text.String()) == "" {
		return Document{}, errors.New("document has no extractable text")
	}
	return Document{Title: title, URL: u.String(), Kind: "google", Version: version, MediaType: "application/json", Text: text.String(), Original: raw}, nil
}
func googleText(value any, b *strings.Builder) {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			googleText(item, b)
		}
	case map[string]any:
		if r, ok := v["textRun"].(map[string]any); ok {
			if s, ok := r["content"].(string); ok {
				b.WriteString(s)
			}
		}
		// Follow document order rather than map iteration; do not include suggestions.
		for _, key := range []string{"body", "content", "elements", "paragraph", "table", "tableRows", "tableCells", "tableOfContents", "tabs", "documentTab", "childTabs"} {
			if c, ok := v[key]; ok {
				googleText(c, b)
			}
		}
	}
}
func (f *Fetcher) github(ctx context.Context, u *url.URL, token string) (Document, error) {
	if token == "" {
		return Document{}, errors.New("run kioku connect github first")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return Document{}, errors.New("expected a GitHub repository, file, issue or pull request URL")
	}
	root := "https://api.github.com/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
	if len(parts) == 2 {
		var r struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
			SHA      string `json:"sha"`
			Name     string `json:"name"`
		}
		if err := f.ProviderGet(ctx, root+"/readme", token, &r); err != nil {
			return Document{}, err
		}
		if r.Encoding != "base64" {
			return Document{}, errors.New("repository README must be a text file")
		}
		b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(r.Content, "\n", ""))
		if err != nil {
			return Document{}, err
		}
		d, err := File(r.Name, string(b))
		d.Title = parts[0] + "/" + parts[1] + " README"
		d.Kind = "github"
		d.URL = u.String()
		d.Version = r.SHA
		return d, err
	}
	if len(parts) == 4 && (parts[2] == "issues" || parts[2] == "pull") {
		n, err := strconv.Atoi(parts[3])
		if err != nil || n < 1 {
			return Document{}, errors.New("invalid issue number")
		}
		return f.githubIssue(ctx, u, token, root, n, parts[2] == "pull")
	}
	if len(parts) < 5 || parts[2] != "blob" {
		return Document{}, errors.New("use a repository root, file, issue or pull request URL")
	}
	owner, repo, ref := parts[0], parts[1], parts[3]
	name := strings.Join(parts[4:], "/")
	for _, s := range []string{owner, repo, ref} {
		if s == "" || s == "." || s == ".." {
			return Document{}, errors.New("invalid GitHub URL")
		}
	}
	api := "https://api.github.com/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/contents/" + strings.Join(escapeParts(parts[4:]), "/") + "?ref=" + url.QueryEscape(ref)
	var r struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		SHA      string `json:"sha"`
		Type     string `json:"type"`
	}
	if err := f.ProviderGet(ctx, api, token, &r); err != nil {
		return Document{}, err
	}
	if r.Encoding != "base64" || r.Type != "file" {
		return Document{}, errors.New("expected a text file within the GitHub API size limit")
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(r.Content, "\n", ""))
	if err != nil {
		return Document{}, err
	}
	d, err := File(name, string(b))
	d.Kind = "github"
	d.URL = u.String()
	d.Version = r.SHA
	return d, err
}
func (f *Fetcher) githubIssue(ctx context.Context, u *url.URL, token, root string, n int, pull bool) (Document, error) {
	var item struct {
		Title    string `json:"title"`
		Body     string `json:"body"`
		State    string `json:"state"`
		Updated  string `json:"updated_at"`
		Comments int    `json:"comments"`
		User     struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := f.ProviderGet(ctx, fmt.Sprintf("%s/issues/%d", root, n), token, &item); err != nil {
		return Document{}, err
	}
	var comments []struct {
		Body    string `json:"body"`
		Updated string `json:"updated_at"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if item.Comments > 0 {
		if err := f.ProviderGet(ctx, fmt.Sprintf("%s/issues/%d/comments?per_page=100", root, n), token, &comments); err != nil {
			return Document{}, err
		}
	}
	var text strings.Builder
	fmt.Fprintf(&text, "# %s\n\nState: %s\nAuthor: %s\nUpdated: %s\n\n%s\n", item.Title, item.State, item.User.Login, item.Updated, item.Body)
	for _, c := range comments {
		fmt.Fprintf(&text, "\n## Comment by %s (%s)\n\n%s\n", c.User.Login, c.Updated, c.Body)
	}
	if item.Comments > len(comments) {
		fmt.Fprintf(&text, "\nOnly the first %d of %d comments are included.\n", len(comments), item.Comments)
	}
	if pull {
		text.WriteString("\nThis snapshot contains the PR description and issue comments. Code diffs and inline review threads are not included.\n")
	}
	raw, err := json.Marshal(map[string]any{"issue": item, "comments": comments})
	if err != nil {
		return Document{}, err
	}
	if len(raw) > MaxBytes {
		return Document{}, errors.New("issue exceeds 4 MiB")
	}
	return Document{Title: item.Title, URL: u.String(), Kind: "github", Version: item.Updated, MediaType: "application/json", Text: text.String(), Original: raw}, nil
}
func escapeParts(parts []string) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = url.PathEscape(p)
	}
	return out
}
func read(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if len(b) > MaxBytes {
		return nil, errors.New("source exceeds 4 MiB")
	}
	return b, err
}
