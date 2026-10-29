// Package webassets serves the static files shared by the bot's web pages
// (/usage, /runs, /problems, /knowledge, /map, /). Everything is embedded in
// the binary: the pages must work in an internal network without any
// external resource.
//
// Vendored:
//   - Apache ECharts 6.1.0 (dist/echarts.min.js, Apache-2.0, see
//     static/ECHARTS-LICENSE.txt and static/ECHARTS-NOTICE.txt), from the
//     6.1.0 tag of github.com/apache/echarts.
//     sha256 b66b25aeb4df84e33199dc21694014d336d222cbd9deb0e5a7c14bd6aa0d0fd0
//
// Own code:
//   - filters.js: the shared filter bar (ReviewFilters; docs/usage-and-diagnostics.md,
//     "Панель фильтров"); its CSS is internal/webui/filters.css, inlined into every page.
//   - rundetail.js: the run detail of /runs/<id> and of the side panel of /runs
//     (RunDetail; needs common.js, loaded after it); its CSS is internal/diagweb/diag.css.
//   - map.js: the container diagram (SVG laid out by elkjs) and the hint
//     snippet of /map (MapGraph; needs common.js and elk, loaded after
//     them); its CSS is internal/mapweb/map.css.
//   - problems.js: the /problems page (ProblemsPage: search without a reload,
//     row selection for "Скрыть выбранные" / "Вернуть выбранные", "Как
//     исправить" toggles, snippet copying; loaded after filters.js and
//     common.js); its CSS is internal/problemsweb/problems.css.
//   - knowledge.js: the /knowledge page (KnowledgePage: the accumulated
//     precision chart, tooltips, expanding decisions, rule selection for the
//     group actions, «скрыть пустые»; loaded after filters.js and common.js);
//     its CSS is internal/knowledgeweb/knowledge.css.
//   - usage.js: the /usage page (UsagePage: renders /usage/data.json — KPIs
//     with deltas, signals, budget, sortable tables, the ECharts charts —
//     and keeps the filter bar in sync with the URL; loaded after
//     echarts, filters.js and common.js); its CSS is internal/usageweb/usage.css.
//   - home.js: the main page "/" (HomePage: writes the hovered hour of a
//     sparkline next to the figure's label; the page works without it);
//     needs nothing else, its CSS is internal/homeweb/home.css.
package webassets

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Prefix is the URL prefix of the assets.
const Prefix = "/assets/"

// EChartsPath is the versioned URL of the ECharts bundle. The version is in
// the name, so it can be cached forever.
const EChartsPath = Prefix + "echarts-6.1.0.min.js"

// ElkPath is the versioned URL of the elkjs bundle (static/elk.bundled.js,
// elkjs 0.9.3 from the npm package, Eclipse Public License 2.0:
// ELK-LICENSE.md): the layered layout of the /map diagram, run in the page
// (the bundle's in-thread worker, no Worker or eval).
const ElkPath = Prefix + "elk-0.9.3.bundled.js"

//go:embed static
var static embed.FS

//go:embed static/filters.js
var filtersJS []byte

//go:embed static/rundetail.js
var runDetailJS []byte

//go:embed static/map.js
var mapJS []byte

//go:embed static/problems.js
var problemsJS []byte

//go:embed static/knowledge.js
var knowledgeJS []byte

//go:embed static/home.js
var homeJS []byte

//go:embed static/usage.js
var usageJS []byte

// FiltersPath is the versioned URL of the filter bar (static/filters.js).
// The version is the start of the file's sha256, so every change gets a new
// URL and the file can be served as immutable.
var FiltersPath = Prefix + "filters-" + contentVersion(filtersJS) + ".js"

// RunDetailPath is the versioned URL of the run-detail renderer
// (static/rundetail.js), versioned like FiltersPath.
var RunDetailPath = Prefix + "rundetail-" + contentVersion(runDetailJS) + ".js"

// MapPath is the versioned URL of the /map graph (static/map.js),
// versioned like FiltersPath.
var MapPath = Prefix + "map-" + contentVersion(mapJS) + ".js"

// ProblemsPath is the versioned URL of the /problems page script
// (static/problems.js), versioned like FiltersPath.
var ProblemsPath = Prefix + "problems-" + contentVersion(problemsJS) + ".js"

// KnowledgePath is the versioned URL of the /knowledge page script
// (static/knowledge.js), versioned like FiltersPath.
var KnowledgePath = Prefix + "knowledge-" + contentVersion(knowledgeJS) + ".js"

// HomePath is the versioned URL of the main page script (static/home.js),
// versioned like FiltersPath.
var HomePath = Prefix + "home-" + contentVersion(homeJS) + ".js"

// UsagePath is the versioned URL of the /usage page script
// (static/usage.js), versioned like FiltersPath.
var UsagePath = Prefix + "usage-" + contentVersion(usageJS) + ".js"

func contentVersion(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

// files maps the public name (after Prefix) to the embedded file.
var files = map[string]string{
	"echarts-6.1.0.min.js": "static/echarts.min.js",
	"ECHARTS-LICENSE.txt":  "static/ECHARTS-LICENSE.txt",
	"ECHARTS-NOTICE.txt":   "static/ECHARTS-NOTICE.txt",
	"elk-0.9.3.bundled.js": "static/elk.bundled.js",
	"ELK-LICENSE.md":       "static/ELK-LICENSE.md",
}

func init() {
	files[strings.TrimPrefix(FiltersPath, Prefix)] = "static/filters.js"
	files[strings.TrimPrefix(RunDetailPath, Prefix)] = "static/rundetail.js"
	files[strings.TrimPrefix(MapPath, Prefix)] = "static/map.js"
	files[strings.TrimPrefix(ProblemsPath, Prefix)] = "static/problems.js"
	files[strings.TrimPrefix(KnowledgePath, Prefix)] = "static/knowledge.js"
	files[strings.TrimPrefix(HomePath, Prefix)] = "static/home.js"
	files[strings.TrimPrefix(UsagePath, Prefix)] = "static/usage.js"
}

// Register mounts the assets on mux. Assets are public library code (no
// data), so they are served without authentication.
func Register(mux *http.ServeMux) {
	mux.Handle(Prefix, Handler())
}

// Handler serves the embedded assets.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, Prefix)
		src, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		b, err := fs.ReadFile(static, src)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		switch path.Ext(name) {
		case ".js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(b)
	})
}
