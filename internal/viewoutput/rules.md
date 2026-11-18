# Kioku View output rules

Submit a gzip-compressed tar archive with these files at its root:

- `index.html` (required): a nonempty UTF-8 HTML document. A minimal document is
  `<!doctype html><html lang="ja"><head><meta charset="utf-8"><title>View</title></head><body><h1>View</h1></body></html>`.
- `summary.md` (optional): UTF-8 Markdown with the key findings and Wiki paths
  as citations.
- `data.json` (optional): a single valid UTF-8 JSON value. Prefer a JSON object
  with meaningful keys for machine readers.
- `assets/` (optional): CSS, PNG, and JPEG files. Names use ASCII letters,
  digits, underscores, hyphens and dots. Start each component with a letter,
  digit or underscore. Use relative paths. Exclude links, duplicates and extra files.

Use semantic HTML and native `details`/`summary` for disclosure. JavaScript,
SVG, forms, iframes and event attributes are prohibited. Use unique attribute names on each element. For metadata use UTF-8 charset, viewport or description.
An `a[href]` can target a local fragment (`#status`) or cite a Wiki page
(`wiki/index.md`). Kioku shows Wiki citations alongside the View. Write external
source addresses as text, or cite their Wiki pages.
Use `img[src]` for bundled PNG/JPEG, and `link[rel="stylesheet"]` for bundled CSS.
A CSS `url(...)` can refer to a bundled image relative to the stylesheet
(or to the document for inline CSS). External URLs and data URLs are prohibited.
Inline `style` attributes accept declarations. Stylesheets and `<style>` support
local layout and animation. Use `@media (max-width: 600px) { .grid { display: block } }`
for responsive layout. CSS may not contain the characters `<`, backslash, or NUL.
Use the listed at-rules and functions below.

Keep the saved prompt focused on what to show, the sources to use and how to
organize the result. A few short paragraphs or bullets are a useful starting
point. Specify any required optional files and keep generated content concise.
