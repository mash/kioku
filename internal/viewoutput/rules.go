// Package viewoutput describes the contract shared by the View generator,
// validator, and Kioku skill.
package viewoutput

import (
	_ "embed"
	"fmt"
	"maps"
	"slices"
	"strings"
)

const (
	MaxArchiveBytes     = 16 << 20
	MaxExpandedBytes    = 8 << 20
	MaxDocumentBytes    = 12 << 20
	MaxTextBytes        = 1 << 20
	MaxCSSBytes         = 256 << 10
	MaxArchiveFiles     = 128
	MaxArchivePathBytes = 180
	MaxHTMLDepth        = 96
	MaxImagePixels      = 16 << 20
	MaxBundlePixels     = 24 << 20
)

//go:embed rules.md
var guide string

var viewElements = viewStringSet(`
	a abbr address article aside b bdi bdo blockquote br caption cite code col colgroup
	data dd del details dfn div dl dt em figcaption figure footer h1 h2 h3 h4 h5 h6
	header hgroup hr i img ins kbd li main mark meter nav ol p pre progress q
	rp rt ruby s samp section small span strong sub summary sup table tbody td th thead
	tfoot time tr u ul var wbr html head body title link meta style`)

var viewGlobalAttrs = viewStringSet(`class id lang dir title role style hidden tabindex`)

var viewElementAttrs = map[string]map[string]bool{
	"a":        {"href": true},
	"col":      {"span": true},
	"colgroup": {"span": true},
	"data":     {"value": true},
	"details":  {"open": true, "name": true},
	"img":      {"src": true, "alt": true, "width": true, "height": true, "loading": true},
	"li":       {"value": true},
	"link":     {"rel": true, "href": true, "media": true},
	"meta":     {"charset": true, "name": true, "content": true},
	"meter":    {"value": true, "min": true, "max": true, "low": true, "high": true, "optimum": true},
	"ol":       {"start": true, "type": true, "reversed": true},
	"progress": {"value": true, "max": true},
	"style":    {"media": true},
	"td":       {"colspan": true, "rowspan": true, "headers": true},
	"th":       {"colspan": true, "rowspan": true, "headers": true, "scope": true, "abbr": true},
	"time":     {"datetime": true},
}

// The only at-rules enabled are local layout and animation rules. Known
// functions either compute a CSS value or select nodes within the document.
var viewCSSAtRules = viewStringSet(`@media @supports @keyframes @-webkit-keyframes @container @layer @starting-style`)
var viewCSSFunctions = viewStringSet(`
	abs attr blur brightness calc clamp color color-mix conic-gradient contrast counter counters
	cubic-bezier drop-shadow env fit-content grayscale hsl hsla hue-rotate hwb invert lab lch
	light-dark linear linear-gradient matrix matrix3d max min minmax mod oklab oklch opacity
	perspective polygon radial-gradient rem repeat repeating-conic-gradient repeating-linear-gradient
	repeating-radial-gradient rgb rgba rotate rotate3d rotatex rotatey rotatez round saturate
	scale scale3d scalex scaley scalez sepia sign skew skewx skewy steps translate translate3d
	translatex translatey translatez var circle ellipse inset path
	dir has is lang not nth-child nth-last-child nth-last-of-type nth-of-type where selector
`)

func viewStringSet(list string) map[string]bool {
	out := make(map[string]bool)
	for _, value := range strings.Fields(list) {
		out[value] = true
	}
	return out
}

func AllowsElement(name string) bool {
	return viewElements[name]
}

var globalAttributePrefixes = []string{"aria-", "data-"}

func AllowsAttribute(element, name string) bool {
	return viewGlobalAttrs[name] || viewElementAttrs[element][name] ||
		slices.ContainsFunc(globalAttributePrefixes, func(prefix string) bool {
			return strings.HasPrefix(name, prefix)
		})
}

func AllowsCSSRule(name string) bool { return viewCSSAtRules[name] }

func AllowsCSSFunction(name string) bool { return viewCSSFunctions[name] }

func Rules() string {
	var out strings.Builder
	out.WriteString(guide)
	fmt.Fprintf(&out, "\nAllowed HTML elements: %s.\n", joinKeys(viewElements))
	fmt.Fprintf(&out, "Global attributes: %s, plus %s prefixes.\n",
		joinKeys(viewGlobalAttrs), strings.Join(globalAttributePrefixes, ", "))
	out.WriteString("Additional attributes by element:\n")
	for _, name := range slices.Sorted(maps.Keys(viewElementAttrs)) {
		fmt.Fprintf(&out, "- %s: %s\n", name, joinKeys(viewElementAttrs[name]))
	}
	fmt.Fprintf(&out, "\nCSS at-rules (stylesheets and style elements): %s.\n", joinKeys(viewCSSAtRules))
	fmt.Fprintf(&out, "CSS functions: %s, and url() for bundled images.\n", joinKeys(viewCSSFunctions))
	fmt.Fprintf(&out, "\nLimits: %d MiB compressed, %d MiB expanded, %d archive entries (files and directories), %d bytes per archive path, "+
		"%d MiB per text file, %d KiB per CSS source, %d MiB rendered document, HTML depth %d, "+
		"%d megapixels per image and %d megapixels total (1 megapixel = 1,048,576 pixels).\n",
		MaxArchiveBytes>>20, MaxExpandedBytes>>20, MaxArchiveFiles, MaxArchivePathBytes, MaxTextBytes>>20,
		MaxCSSBytes>>10, MaxDocumentBytes>>20, MaxHTMLDepth, MaxImagePixels>>20, MaxBundlePixels>>20)
	return out.String()
}

func joinKeys(values map[string]bool) string {
	return strings.Join(slices.Sorted(maps.Keys(values)), ", ")
}
