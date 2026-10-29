package diffcov

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Changed maps a repository path (new path) to the line numbers the MR
// added or changed in the new version of the file.
type Changed map[string][]int

// ChangedFromHunks reads GitLab /diffs hunks (no file headers) of one file.
func ChangedFromHunks(diff string) []int {
	var out []int
	line := 0
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			line = hunkNewStart(l)
		case line == 0:
		case strings.HasPrefix(l, "+"):
			out = append(out, line)
			line++
		case strings.HasPrefix(l, "-"), strings.HasPrefix(l, `\`):
		default:
			line++
		}
	}
	return out
}

// ChangedFromUnified reads a git unified diff with file headers
// (`git diff base...HEAD`).
func ChangedFromUnified(diff string) Changed {
	ch := Changed{}
	var file string
	var lines []int
	line := 0
	flush := func() {
		if file != "" && len(lines) > 0 {
			ch[file] = append(ch[file], lines...)
		}
		lines = nil
	}
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			flush()
			file, line = "", 0
		case strings.HasPrefix(l, "+++ "):
			p := strings.TrimPrefix(l, "+++ ")
			if p == "/dev/null" {
				file = ""
			} else {
				file = strings.TrimPrefix(p, "b/")
			}
		case strings.HasPrefix(l, "--- "):
		case strings.HasPrefix(l, "@@"):
			line = hunkNewStart(l)
		case line == 0 || file == "":
		case strings.HasPrefix(l, "+"):
			lines = append(lines, line)
			line++
		case strings.HasPrefix(l, "-"), strings.HasPrefix(l, `\`):
		default:
			line++
		}
	}
	flush()
	return ch
}

// hunkNewStart parses "@@ -a,b +c,d @@" and returns c.
func hunkNewStart(h string) int {
	i := strings.Index(h, "+")
	if i < 0 {
		return 0
	}
	f := strings.Fields(h[i+1:])
	if len(f) == 0 {
		return 0
	}
	s, _, _ := strings.Cut(f[0], ",")
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// Range is an inclusive line range.
type Range struct{ From, To int }

func (r Range) String() string {
	if r.From == r.To {
		return strconv.Itoa(r.From)
	}
	return fmt.Sprintf("%d-%d", r.From, r.To)
}

// FileResult is the diff coverage of one file.
type FileResult struct {
	Path       string  `json:"path"`
	ReportPath string  `json:"report_path"`
	Covered    int     `json:"covered"`
	Total      int     `json:"total"` // changed executable lines
	Uncovered  []Range `json:"uncovered"`
}

// Percent of covered changed lines (100 when Total is 0).
func (f FileResult) Percent() float64 { return percent(f.Covered, f.Total) }

// Result is the diff coverage of an MR.
type Result struct {
	Covered int          `json:"covered"`
	Total   int          `json:"total"`
	Files   []FileResult `json:"files"`
	// NotInReport are changed source files the report does not mention
	// (excluded from coverage, not run by the tests, or a path mismatch).
	NotInReport []string `json:"not_in_report"`
}

// Percent of covered changed lines (100 when Total is 0).
func (r Result) Percent() float64 { return percent(r.Covered, r.Total) }

func percent(c, t int) float64 {
	if t == 0 {
		return 100
	}
	return float64(c) * 100 / float64(t)
}

// sourceExt are the files listed in NotInReport when missing from the
// report (other changed files — docs, configs — are ignored).
var sourceExt = map[string]bool{".go": true, ".py": true, ".php": true, ".js": true, ".jsx": true, ".ts": true,
	".tsx": true, ".java": true, ".kt": true, ".rb": true, ".cs": true, ".rs": true, ".scala": true, ".swift": true}

// Compute matches changed files to report files and counts covered
// changed lines. include, when non-nil, limits the files considered.
func Compute(rep *Report, ch Changed, include func(path string) bool) Result {
	var res Result
	paths := make([]string, 0, len(ch))
	for p := range ch {
		if include == nil || include(p) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		rp, ok := match(rep, p)
		if !ok {
			if sourceExt[strings.ToLower(path.Ext(p))] && !isTestFile(p) {
				res.NotInReport = append(res.NotInReport, p)
			}
			continue
		}
		fr := fileResult(p, rp, ch[p], rep.Files[rp])
		if fr.Total == 0 {
			continue
		}
		res.Covered += fr.Covered
		res.Total += fr.Total
		res.Files = append(res.Files, fr)
	}
	return res
}

// fileResult counts the covered changed lines of one file (lines: the
// report's executable lines of it).
func fileResult(p, rp string, changed []int, lines map[int]int) FileResult {
	fr := FileResult{Path: p, ReportPath: rp}
	var unc []int
	seen := map[int]bool{}
	for _, ln := range changed {
		if seen[ln] {
			continue
		}
		seen[ln] = true
		hits, exec := lines[ln]
		if !exec {
			continue
		}
		fr.Total++
		if hits > 0 {
			fr.Covered++
		} else {
			unc = append(unc, ln)
		}
	}
	fr.Uncovered = ranges(unc, lines)
	return fr
}

// ranges merges uncovered lines into ranges; a gap of non-executable lines
// (comments, blank) does not split a range.
func ranges(lines []int, exec map[int]int) []Range {
	sort.Ints(lines)
	var out []Range
	for _, ln := range lines {
		if n := len(out); n > 0 {
			last := &out[n-1]
			contiguous := true
			for x := last.To + 1; x < ln; x++ {
				if _, ok := exec[x]; ok {
					contiguous = false
					break
				}
			}
			if contiguous {
				last.To = ln
				continue
			}
		}
		out = append(out, Range{ln, ln})
	}
	return out
}

// match finds the report file for a repository path: exact, or the report
// path ends with "/"+repoPath (absolute runner paths), or the repository
// path ends with "/"+reportPath (paths relative to a source root). The
// candidate sharing the longest suffix wins; among report paths that all
// end with the whole repository path, the shortest one.
func match(rep *Report, repoPath string) (string, bool) {
	repoPath = path.Clean(repoPath)
	if _, ok := rep.Files[repoPath]; ok {
		return repoPath, true
	}
	best, bestLen := "", 0
	for rp := range rep.Files {
		var n int
		switch {
		case strings.HasSuffix(rp, "/"+repoPath):
			n = len(repoPath)
		case strings.HasSuffix(repoPath, "/"+rp):
			n = len(rp)
		default:
			continue
		}
		// Equal suffixes (several report paths end with "/"+repoPath):
		// the shortest report path wins, so "main.go" is /b/p/main.go
		// rather than /b/p/cmd/x/main.go.
		if n > bestLen || (n == bestLen && (len(rp) < len(best) || len(rp) == len(best) && rp < best)) {
			best, bestLen = rp, n
		}
	}
	return best, best != ""
}

func isTestFile(p string) bool {
	b := strings.ToLower(path.Base(p))
	switch {
	case strings.HasSuffix(b, "_test.go"), strings.HasPrefix(b, "test_") && strings.HasSuffix(b, ".py"),
		strings.HasSuffix(b, "_test.py"), strings.HasSuffix(b, "test.php"),
		strings.Contains(b, ".test."), strings.Contains(b, ".spec."):
		return true
	}
	for _, seg := range strings.Split(strings.ToLower(p), "/") {
		if seg == "tests" || seg == "test" || seg == "__tests__" {
			return true
		}
	}
	return false
}
