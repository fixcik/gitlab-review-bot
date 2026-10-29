package sysmap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/goccy/go-yaml"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/config"
)

// codeExts are the languages the extraction looks at.
var codeExts = map[string]bool{
	".php": true, ".py": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".ts": true, ".tsx": true, ".vue": true, ".go": true,
}

// skipDirs are directory names never walked (dependencies, build output,
// caches).
var skipDirs = map[string]bool{
	"vendor": true, "node_modules": true, "bower_components": true, "dist": true, "build": true, "out": true,
	"coverage": true, "__pycache__": true, "venv": true, "site-packages": true,
	"storybook-static": true, "var": true, "storage": true, "tmp": true,
}

// noCodeDirs hold no contract code worth extracting (tests, generated
// code, docs, static files) but may hold OpenAPI specs.
var noCodeDirs = map[string]bool{
	"test": true, "tests": true, "__tests__": true, "__mocks__": true, "spec": true, "specs": true,
	"fixtures": true, "testdata": true, "e2e": true, "cypress": true, "playwright": true,
	"generated": true, "gen": true, "docs": true, "migrations": true, "public": true, "static": true, "assets": true,
}

func inNoCodeDir(rel string) bool {
	segs := strings.Split(rel, "/")
	for _, s := range segs[:len(segs)-1] {
		if noCodeDirs[strings.ToLower(s)] {
			return true
		}
	}
	return false
}

// mockTokens are words of a path segment (split at "-", "_", ".") that
// mark mocks, stubs, fakes, fixtures and examples; testDirs are test
// directories. Endpoints declared there are not providers (mockPath).
var (
	mockTokens = map[string]bool{
		"mock": true, "mocks": true, "stub": true, "stubs": true, "fake": true, "fakes": true,
		"fixture": true, "fixtures": true, "testdata": true, "sandbox": true, "example": true, "examples": true,
	}
	testDirs = map[string]bool{"test": true, "tests": true, "__tests__": true, "e2e": true}
)

// mockPath reports a file of a mock, stub or fake server, a fixture, an
// example or a test (deploy/env/local/mock-api/server.js, src/__mocks__/,
// tests/, server.mock.js): its routes only imitate another service, so they
// never provide an endpoint in the map. Whole words only ("mockingbird",
// "contest" are not), and spec/ is not a test directory (OpenAPI files
// live there).
func mockPath(rel string) bool {
	segs := strings.Split(strings.ToLower(rel), "/")
	for i, s := range segs {
		if i == len(segs)-1 {
			s = strings.TrimSuffix(s, path.Ext(s))
		} else if testDirs[s] {
			return true
		}
		for _, w := range strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
			if mockTokens[w] {
				return true
			}
		}
	}
	return false
}

// skipFileRe matches tests, minified, generated and declaration files.
var skipFileRe = regexp.MustCompile(`(?i)(\.min\.|\.bundle\.|\.d\.ts$|_test\.go$|\.pb\.go$|_pb2(_grpc)?\.py$|\.test\.|\.spec\.|\.stories\.|\.generated\.|\.gen\.|^test_.*\.py$|_test\.py$|^conftest\.py$|Test\.php$)`)

// hintRe finds files that may define or use a contract: route
// declarations, HTTP clients, WebSocket/SSE, webhooks and URL literals.
// Framework-agnostic on purpose: broad patterns, the model decides.
var hintRe = regexp.MustCompile(`(?i)(` +
	// HTTP clients
	`\bfetch\s*\(|\baxios\b|\$http\b|httpclient|guzzle|curl_(init|exec|setopt)|\brequests\.(get|post|put|patch|delete|request|session)\b|\bhttpx\b|aiohttp|urllib|` +
	`\bhttp\.(get|post|newrequest|newrequestwithcontext)\b|->(get|post|put|patch|delete)\s*\(\s*['"]/|->request\s*\(\s*['"](get|post|put|patch|delete)['"]|` +
	`\bhttp::|\bky\b|superagent|\$\.(ajax|get|post)\b|useFetch|\$fetch|` +
	// routes
	`@(get|post|put|patch|delete|route|requestmapping|api_view)\b|@(app|router|api|bp|blueprint)\.|#\[route|\broute::|\brouter\.|\.route\s*\(|` +
	`\bapp\.(get|post|put|patch|delete|use|websocket|api_route)\b|\burlpatterns\b|\bre_path\s*\(|\bpath\s*\(\s*['"]|apirouter|blueprint|handlefunc|\.handle\s*\(|` +
	`\bmux\.|\bchi\.|\bgin\.|\becho\.|\bfiber\.|` +
	// websocket / sse
	`websocket|socket\.io|\b(socket|io|ws|client|server|channel)\.(on|emit|send)\s*\(|\bio\s*\(|eventsource|text/event-stream|server-sent|streamingresponse|eventsourceresponse|\bsse\b|` +
	`\bbroadcast|pusher|centrifug|` +
	// webhooks
	`webhook|callback_url|` +
	// URL literals
	`['"` + "`" + `]https?://|['"` + "`" + `]/api/|['"` + "`" + `]/v[0-9]+/` +
	`)`)

// Spec discovery limits.
const (
	maxSpecs     = 20
	maxSpecBytes = 4 << 20
)

// candidate is a file selected for extraction.
type candidate struct {
	Path string
	Hash string
	Size int64
	Spec bool
}

// selection is the result of walking one snapshot.
type selection struct {
	Code     []candidate
	Specs    []candidate
	Capped   int
	TooLarge int
}

// selectFiles walks a snapshot directory and picks the code candidates
// (extension, not excluded, regex hint, size cap) and the OpenAPI specs.
// Symlinks and hidden directories are skipped. maxFiles caps the code
// candidates (sorted by path, stable across passes).
func selectFiles(root string, maxFiles int, maxBytes int64, exclude []string) (selection, error) {
	var sel selection
	rc := readRepoConfig(root)
	exclude = append(append([]string(nil), exclude...), rc.exclude...)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		name := d.Name()
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(name, ".") || skipDirs[strings.ToLower(name)] || excluded(exclude, rel) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || excluded(exclude, rel) {
			return nil
		}
		ext := strings.ToLower(path.Ext(name))
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		switch {
		case specExt(ext):
			if len(sel.Specs) >= maxSpecs || info.Size() > maxSpecBytes || !specCandidateName(rel) {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil || !compat.LooksLikeOpenAPI(b) {
				return nil
			}
			sel.Specs = append(sel.Specs, candidate{Path: rel, Hash: hashOf(b), Size: info.Size(), Spec: true})
		case codeExts[ext]:
			if skipFileRe.MatchString(name) || inNoCodeDir(rel) {
				return nil
			}
			if info.Size() > maxBytes {
				sel.TooLarge++
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil || bytes.IndexByte(b, 0) >= 0 || !hintRe.Match(b) {
				return nil
			}
			sel.Code = append(sel.Code, candidate{Path: rel, Hash: hashOf(b), Size: info.Size()})
		}
		return nil
	})
	if err != nil {
		return sel, err
	}
	sort.Slice(sel.Code, func(i, j int) bool { return sel.Code[i].Path < sel.Code[j].Path })
	sort.Slice(sel.Specs, func(i, j int) bool { return sel.Specs[i].Path < sel.Specs[j].Path })
	if maxFiles > 0 && len(sel.Code) > maxFiles {
		sel.Capped = len(sel.Code) - maxFiles
		sel.Code = sel.Code[:maxFiles]
	}
	return sel, nil
}

// specExt: an extension an OpenAPI spec may have.
func specExt(ext string) bool { return ext == ".yaml" || ext == ".yml" || ext == ".json" }

// specCandidateName: files worth sniffing for a top-level openapi/swagger
// key. Lock files and well-known configs are skipped without reading.
func specCandidateName(rel string) bool {
	base := strings.ToLower(path.Base(rel))
	switch base {
	case "package.json", "package-lock.json", "composer.json", "composer.lock", "tsconfig.json", "jsconfig.json",
		".reviewbot.yaml", ".gitlab-ci.yml", "docker-compose.yml", "docker-compose.yaml", "pnpm-lock.yaml", "yarn.lock":
		return false
	}
	return true
}

func excluded(globs []string, rel string) bool {
	for _, g := range globs {
		if ok, err := doublestar.Match(g, rel); err == nil && ok {
			return true
		}
	}
	return false
}

func hashOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// repoConfig is what the scanner takes from the snapshot's .reviewbot.yaml:
// sysmap.exclude (file globs not to scan).
type repoConfig struct {
	exclude []string
}

func readRepoConfig(root string) repoConfig {
	p := filepath.Join(root, config.RepoConfigPath)
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() || fi.Size() > 256<<10 {
		return repoConfig{}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return repoConfig{}
	}
	rc, err := config.ParseRepo(b)
	if err == nil {
		var out repoConfig
		if rc.Sysmap != nil {
			out.exclude = rc.Sysmap.Exclude
		}
		return out
	}
	// The strict parser refuses the whole file for an unrelated problem
	// (an unknown or removed key elsewhere): the map still honours
	// sysmap.exclude, so files the repository excluded are never sent to
	// the model. Only valid globs are taken.
	var lenient struct {
		Sysmap struct {
			Exclude []string `yaml:"exclude"`
		} `yaml:"sysmap"`
	}
	if yaml.Unmarshal(b, &lenient) != nil {
		return repoConfig{}
	}
	var out repoConfig
	for _, g := range lenient.Sysmap.Exclude {
		if g = strings.Trim(strings.TrimSpace(g), "/"); g != "" && doublestar.ValidatePattern(g) {
			out.exclude = append(out.exclude, g)
		}
	}
	return out
}
