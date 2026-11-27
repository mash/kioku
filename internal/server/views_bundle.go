package server

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/gorilla/css/scanner"
	"github.com/mash/kioku/internal/viewoutput"
	"github.com/mash/kioku/internal/wiki"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var (
	errViewArchive = errors.New("View のアーカイブ形式が無効です")
	errViewLimit   = errors.New("View の成果物が上限を超えています")
	errViewEntry   = errors.New("View のファイル名または形式が無効です")
	errViewHTML    = errors.New("許可されていない HTML 要素または属性")
	errViewAsset   = errors.New("View の参照先は同梱ファイルまたは Wiki に限ります")
	errViewCSS     = errors.New("許可されていない CSS")
)

type viewCitation struct {
	Path  string
	Label string
}

type viewBundle struct {
	files     map[string][]byte
	document  string
	citations []viewCitation
}

func (b *viewBundle) Document() string          { return b.document }
func (b *viewBundle) Citations() []viewCitation { return b.citations }

var viewTextPaths = []string{"index.html", "summary.md", "data.json"}

func (b *viewBundle) Archive() ([]byte, error) {
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for _, name := range slices.Sorted(maps.Keys(b.files)) {
		entry, err := w.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := entry.Write(b.files[name]); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type viewEntries struct {
	files map[string][]byte
	seen  map[string]bool
	bytes int64
}

func newViewEntries() *viewEntries {
	return &viewEntries{files: make(map[string][]byte), seen: make(map[string]bool)}
}

func readViewBundle(body []byte) (*viewBundle, error) {
	if len(body) > viewoutput.MaxArchiveBytes {
		return nil, errViewLimit
	}
	zipFile, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, errViewArchive
	}
	entries := newViewEntries()
	for _, file := range zipFile.File {
		mode := file.Mode()
		if !mode.IsDir() && !mode.IsRegular() {
			return nil, errViewEntry
		}
		if file.UncompressedSize64 > viewoutput.MaxExpandedBytes {
			return nil, errViewLimit
		}
		if err := entries.add(file.Name, int64(file.UncompressedSize64), mode.IsDir(), file.Open); err != nil {
			return nil, err
		}
	}
	return validateViewEntries(entries.files)
}

// A retrieved bundle has already passed validation and matched the committed
// hash. Read just the requested text entry for each MCP page.
func readPublishedViewText(body []byte, name string) (string, error) {
	if !slices.Contains(viewTextPaths, name) {
		return "", fail(400, "invalid View path")
	}
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", errViewArchive
	}
	for _, file := range archive.File {
		if file.Name != name {
			continue
		}
		r, err := file.Open()
		if err != nil {
			return "", errViewArchive
		}
		defer r.Close()
		text, err := io.ReadAll(io.LimitReader(r, viewoutput.MaxTextBytes+1))
		if err != nil || len(text) > viewoutput.MaxTextBytes || !utf8.Valid(text) {
			return "", errViewEntry
		}
		return string(text), nil
	}
	return "", fail(404, "View file not found")
}

func readViewRunnerOutput(body []byte) (*viewBundle, error) {
	if len(body) > viewoutput.MaxArchiveBytes {
		return nil, errViewLimit
	}
	compressed := bytes.NewReader(body)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return nil, errViewArchive
	}
	defer gz.Close()
	gz.Multistream(false)
	// Leave room for tar headers and padding, even for bundles with many files.
	expanded := &io.LimitedReader{R: gz, N: viewoutput.MaxExpandedBytes + (2 << 20) + 1}
	tr := tar.NewReader(expanded)
	entries := newViewEntries()
	rootDirectory := true
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if expanded.N == 0 {
				return nil, errViewLimit
			}
			return nil, errViewArchive
		}
		dir := header.Typeflag == tar.TypeDir
		if !dir && header.Typeflag != tar.TypeReg {
			return nil, errViewEntry
		}
		name := strings.TrimPrefix(header.Name, "./")
		if dir && (name == "." || name == "") {
			if header.Size != 0 || !rootDirectory {
				return nil, errViewEntry
			}
			rootDirectory = false
			continue
		}
		rootDirectory = false
		if err := entries.add(name, header.Size, dir, func() (io.ReadCloser, error) {
			return io.NopCloser(tr), nil
		}); err != nil {
			return nil, err
		}
	}
	// Drain gzip to verify its checksum. A conventional tar may leave padding.
	var padding [4096]byte
	for {
		n, err := expanded.Read(padding[:])
		if expanded.N == 0 {
			return nil, errViewLimit
		}
		for _, b := range padding[:n] {
			if b != 0 {
				return nil, errViewArchive
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errViewArchive
		}
	}
	if compressed.Len() != 0 {
		return nil, errViewArchive
	}
	return validateViewEntries(entries.files)
}

var viewAssetPath = regexp.MustCompile(`^assets(?:/[A-Za-z0-9_][A-Za-z0-9_.-]*)*$`)

func (e *viewEntries) add(name string, size int64, dir bool, open func() (io.ReadCloser, error)) error {
	if len(e.seen) >= viewoutput.MaxArchiveFiles {
		return errViewLimit
	}
	if dir {
		name = strings.TrimSuffix(name, "/")
	}
	if name == "" || len(name) > viewoutput.MaxArchivePathBytes || e.seen[name] || name != path.Clean(name) {
		return errViewEntry
	}
	e.seen[name] = true
	if dir {
		if size != 0 || !viewAssetPath.MatchString(name) {
			return errViewEntry
		}
		return nil
	}

	var fileLimit int64 = viewoutput.MaxExpandedBytes
	switch {
	case slices.Contains(viewTextPaths, name):
		fileLimit = viewoutput.MaxTextBytes
	case strings.HasPrefix(name, "assets/") && viewAssetPath.MatchString(name):
		switch strings.ToLower(path.Ext(name)) {
		case ".css":
			fileLimit = viewoutput.MaxCSSBytes
		case ".png", ".jpg", ".jpeg":
		default:
			return errViewEntry
		}
	default:
		return errViewEntry
	}
	if size < 0 || size > fileLimit || size > viewoutput.MaxExpandedBytes-e.bytes {
		return errViewLimit
	}
	r, err := open()
	if err != nil {
		return errViewArchive
	}
	defer r.Close()
	body, err := io.ReadAll(io.LimitReader(r, size+1))
	if err != nil || int64(len(body)) != size {
		return errViewArchive
	}
	e.files[name] = body
	e.bytes += size
	return nil
}

type viewDocument struct {
	images    map[string]string
	sheets    map[string]string
	citations []viewCitation
	seenLinks map[string]bool
	bytes     int
}

func validateViewEntries(files map[string][]byte) (*viewBundle, error) {
	entry, ok := files["index.html"]
	if !ok || len(bytes.TrimSpace(entry)) == 0 {
		return nil, fmt.Errorf("index.html: %w (必須のファイルが空か存在しません)", errViewEntry)
	}
	for _, name := range viewTextPaths {
		body, exists := files[name]
		if !exists {
			continue
		}
		if !utf8.Valid(body) {
			return nil, fmt.Errorf("%s: %w (UTF-8 が必要です)", name, errViewEntry)
		}
		if name == "data.json" && !json.Valid(body) {
			return nil, fmt.Errorf("%s: %w (有効な JSON が必要です)", name, errViewEntry)
		}
	}
	doc := &viewDocument{
		images:    make(map[string]string),
		sheets:    make(map[string]string),
		seenLinks: make(map[string]bool),
		bytes:     len(entry),
	}
	pixelsRemaining := viewoutput.MaxBundlePixels
	names := slices.Sorted(maps.Keys(files))
	for _, name := range names {
		body := files[name]
		switch strings.ToLower(path.Ext(name)) {
		case ".png", ".jpg", ".jpeg":
			format, pixels, err := validateViewImage(name, body, pixelsRemaining)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			pixelsRemaining -= pixels
			doc.images[name] = "data:image/" + format + ";base64," + base64.StdEncoding.EncodeToString(body)
		}
	}
	for _, name := range names {
		if strings.EqualFold(path.Ext(name), ".css") {
			css, err := doc.rewriteCSS(string(files[name]), path.Dir(name), true)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if err := doc.addDocumentBytes(len(css)); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			doc.sheets[name] = css
		}
	}
	document, err := doc.render(entry)
	if err != nil {
		return nil, fmt.Errorf("index.html: %w", err)
	}
	return &viewBundle{files: files, document: document, citations: doc.citations}, nil
}

func validateViewImage(name string, body []byte, pixelsRemaining int) (string, int, error) {
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".")
	if ext == "jpg" {
		ext = "jpeg"
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || format != ext || config.Width <= 0 || config.Height <= 0 {
		return "", 0, errViewEntry
	}
	if config.Width > min(viewoutput.MaxImagePixels, pixelsRemaining)/config.Height {
		return "", 0, errViewLimit
	}
	if _, _, err := image.Decode(bytes.NewReader(body)); err != nil {
		return "", 0, errViewEntry
	}
	return format, config.Width * config.Height, nil
}

func (d *viewDocument) render(entry []byte) (string, error) {
	tokens := html.NewTokenizer(bytes.NewReader(entry))
	for tag := tokens.Next(); tag != html.ErrorToken; tag = tokens.Next() {
		switch tag {
		case html.StartTagToken, html.SelfClosingTagToken:
			t := tokens.Token()
			if err := checkViewHTML(t.Data, t.Attr); err != nil {
				return "", err
			}
		}
	}

	if tokens.Err() != io.EOF {
		return "", errViewHTML
	}
	root, err := html.Parse(bytes.NewReader(entry))
	if err != nil {
		return "", errViewHTML
	}
	if err := d.rewriteHTML(root, 0); err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := html.Render(&out, root); err != nil {
		return "", err
	}
	if out.Len() > viewoutput.MaxDocumentBytes {
		return "", errViewLimit
	}
	return out.String(), nil
}

// Report only short syntax names, without attribute values or source snippets.
func viewSyntaxName(name string) string {
	if len(name) > 48 {
		return "(名前が長すぎます)"
	}
	return fmt.Sprintf("%q", name)
}

func checkViewHTML(element string, attrs []html.Attribute) error {
	if !viewoutput.AllowsElement(element) {
		return fmt.Errorf("%w (要素 %s)", errViewHTML, viewSyntaxName(element))
	}
	names := make(map[string]bool, len(attrs))
	for _, a := range attrs {
		if names[a.Key] {
			return fmt.Errorf("%w (要素 %s の重複した属性 %s)", errViewHTML, element, viewSyntaxName(a.Key))
		}
		if a.Namespace != "" || !viewoutput.AllowsAttribute(element, a.Key) {
			return fmt.Errorf("%w (要素 %s の属性 %s)", errViewHTML, element, viewSyntaxName(a.Key))
		}
		names[a.Key] = true
	}
	return nil
}

const viewFrameCSP = "default-src 'none'; img-src data:; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'"

func (d *viewDocument) rewriteHTML(n *html.Node, depth int) error {
	if depth > viewoutput.MaxHTMLDepth {
		return errViewLimit
	}
	if n.Type == html.ElementNode {
		if n.Namespace != "" {
			return fmt.Errorf("%w (要素 %s の名前空間)", errViewHTML, viewSyntaxName(n.Data))
		}
		if err := checkViewHTML(n.Data, n.Attr); err != nil {
			return err
		}
		switch n.DataAtom {
		case atom.Meta:
			if !strings.EqualFold(viewAttr(n, "charset"), "utf-8") &&
				!slices.Contains([]string{"viewport", "description"}, strings.ToLower(viewAttr(n, "name"))) {
				return fmt.Errorf("%w (meta の charset または name)", errViewHTML)
			}
		case atom.Img:
			imageURL := d.images[viewAssetName(".", viewAttr(n, "src"))]
			if imageURL == "" {
				return fmt.Errorf("%w (img の src)", errViewAsset)
			}
			if err := d.addDocumentBytes(len(imageURL)); err != nil {
				return err
			}
			setViewAttr(n, "src", imageURL)
		case atom.Link:
			sheet := viewAssetName(".", viewAttr(n, "href"))
			css, exists := d.sheets[sheet]
			if !strings.EqualFold(viewAttr(n, "rel"), "stylesheet") || !exists {
				return fmt.Errorf("%w (link の rel または href)", errViewAsset)
			}
			if err := d.addDocumentBytes(len(css)); err != nil {
				return err
			}
			media := viewAttr(n, "media")
			n.Data, n.DataAtom, n.Attr = "style", atom.Style, nil
			if media != "" {
				setViewAttr(n, "media", media)
			}
			n.AppendChild(&html.Node{Type: html.TextNode, Data: css})
			return nil
		case atom.Style:
			if child := n.FirstChild; child != nil {
				css, err := d.rewriteCSS(child.Data, ".", true)
				if err != nil {
					return fmt.Errorf("style 要素: %w", err)
				}
				if err := d.addDocumentBytes(len(css)); err != nil {
					return err
				}
				child.Data = css
			}
		case atom.A:
			if err := d.rewriteLink(n); err != nil {
				return fmt.Errorf("%w (a の href)", err)
			}
		}
		if style := viewAttr(n, "style"); style != "" {
			css, err := d.rewriteCSS(style, ".", false)
			if err != nil {
				return fmt.Errorf("%s の style 属性: %w", n.Data, err)
			}
			if err := d.addDocumentBytes(len(css)); err != nil {
				return err
			}
			setViewAttr(n, "style", css)
		}
	}
	for child := n.FirstChild; child != nil; {
		next := child.NextSibling
		if child.Type == html.CommentNode {
			n.RemoveChild(child)
		} else if err := d.rewriteHTML(child, depth+1); err != nil {
			return err
		}
		child = next
	}
	if n.DataAtom == atom.Head {
		n.InsertBefore(&html.Node{
			Type: html.ElementNode, Data: "meta", DataAtom: atom.Meta,
			Attr: []html.Attribute{
				{Key: "http-equiv", Val: "Content-Security-Policy"},
				{Key: "content", Val: viewFrameCSP},
			},
		}, n.FirstChild)
	}
	return nil
}

func (d *viewDocument) addDocumentBytes(n int) error {
	d.bytes += n
	if d.bytes > viewoutput.MaxDocumentBytes {
		return errViewLimit
	}
	return nil
}

func viewAttr(n *html.Node, key string) string {
	for _, attr := range n.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func setViewAttr(n *html.Node, key, value string) {
	for i := range n.Attr {
		if n.Attr[i].Key == key {
			n.Attr[i].Val = value
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: value})
}

func viewAssetName(base, raw string) string {
	ref := strings.TrimSpace(raw)
	if ref == "" || strings.HasPrefix(ref, "/") || strings.ContainsAny(ref, ":%?#\\") {
		return ""
	}
	name := path.Join(base, ref)
	if !strings.HasPrefix(name, "assets/") || !viewAssetPath.MatchString(name) {
		return ""
	}
	return name
}

func (d *viewDocument) rewriteLink(n *html.Node) error {
	raw := strings.TrimSpace(viewAttr(n, "href"))
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host != "" || u.Scheme != "" || u.User != nil || strings.HasPrefix(raw, "//") {
		return errViewAsset
	}
	if strings.HasPrefix(raw, "#") {
		// A relative href in srcdoc would resolve against the parent URL.
		setViewAttr(n, "href", "about:srcdoc#"+u.EscapedFragment())
		return nil
	}
	file := u.Path
	if file == "" && u.RawQuery != "" {
		query, err := url.ParseQuery(u.RawQuery)
		if err != nil || len(query) != 1 || len(query["file"]) != 1 {
			return errViewAsset
		}
		file = query.Get("file")
	} else if u.RawQuery != "" {
		return errViewAsset
	}
	if !strings.HasPrefix(file, "wiki/") || !wiki.ValidFile(file) {
		return errViewAsset
	}
	if !d.seenLinks[file] {
		label := strings.Join(strings.Fields(viewNodeText(n)), " ")
		if label == "" || len(label) > 512 {
			label = file
		}
		d.citations = append(d.citations, viewCitation{Path: file, Label: label})
		d.seenLinks[file] = true
	}
	n.Attr = slices.DeleteFunc(n.Attr, func(a html.Attribute) bool { return a.Key == "href" })
	return nil
}

func viewNodeText(root *html.Node) string {
	var out strings.Builder
	// Descendants iterates without recursing and excludes the root itself.
	for n := range root.Descendants() {
		if n.Type == html.TextNode {
			out.WriteString(n.Data)
		}
	}
	return out.String()
}

func (d *viewDocument) rewriteCSS(source, base string, stylesheet bool) (string, error) {
	if len(source) > viewoutput.MaxCSSBytes {
		return "", errViewLimit
	}
	// CSS escapes can hide a function name from the tokenizer. Reject them
	// before scanning, and keep raw HTML delimiters out of style elements.
	if !utf8.ValidString(source) || strings.ContainsAny(source, "<\\\x00") {
		return "", errViewCSS
	}
	var out strings.Builder
	var closures []byte
	scan := scanner.New(source)
	for {
		t := scan.Next()
		value := t.Value
		switch t.Type {
		case scanner.TokenEOF:
			if len(closures) != 0 {
				return "", errViewCSS
			}
			return out.String(), nil
		case scanner.TokenError, scanner.TokenCDO, scanner.TokenCDC:
			return "", errViewCSS
		case scanner.TokenAtKeyword:
			if !stylesheet || !viewoutput.AllowsCSSRule(strings.ToLower(value)) {
				return "", fmt.Errorf("%w (規則 %s)", errViewCSS, viewSyntaxName(value))
			}
		case scanner.TokenURI:
			ref := strings.Trim(strings.TrimSpace(value[len("url("):len(value)-1]), "'\"")
			imageURL := d.images[viewAssetName(base, ref)]
			if imageURL == "" {
				return "", fmt.Errorf("%w (CSS の url())", errViewAsset)
			}
			value = "url(\"" + imageURL + "\")"
		case scanner.TokenFunction:
			name := strings.ToLower(strings.TrimSuffix(value, "("))
			if !viewoutput.AllowsCSSFunction(name) {
				return "", fmt.Errorf("%w (関数 %s)", errViewCSS, viewSyntaxName(name))
			}
			closures = append(closures, ')')
		case scanner.TokenChar:
			switch value {
			case "{":
				if !stylesheet {
					return "", errViewCSS
				}
				closures = append(closures, '}')
			case "(":
				closures = append(closures, ')')
			case "[":
				closures = append(closures, ']')
			case "}", ")", "]":
				if len(closures) == 0 || closures[len(closures)-1] != value[0] {
					return "", errViewCSS
				}
				closures = closures[:len(closures)-1]
			}
		}
		if len(value) > viewoutput.MaxDocumentBytes-out.Len() || len(closures) > viewoutput.MaxHTMLDepth {
			return "", errViewLimit
		}
		out.WriteString(value)
	}
}
