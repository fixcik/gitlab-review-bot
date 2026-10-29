// Package diffcov computes test coverage of the lines an MR adds or changes
// ("diff coverage") from a coverage report produced by CI: Cobertura XML
// (coverage.py, PHPUnit --coverage-cobertura, gocover-cobertura), Clover
// XML (PHPUnit --coverage-clover), Go cover profiles (go test
// -coverprofile) and LCOV.
//
// Paths in CI reports are usually absolute runner paths
// (/builds/group/project/src/x.php) or relative to a source root; they are
// matched to repository paths by the longest common path suffix.
package diffcov

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// Format of a coverage report.
type Format string

const (
	FormatCobertura Format = "cobertura"
	FormatClover    Format = "clover"
	FormatGo        Format = "go"
	FormatLCOV      Format = "lcov"
)

// MaxReportBytes caps the report size accepted by Parse.
const MaxReportBytes = 100 << 20

// Report maps a report path (slash-separated, as written in the report) to
// its executable lines and their hit counts. Lines absent from the map are
// not executable (comments, blank lines, declarations).
type Report struct {
	Format Format
	Files  map[string]map[int]int
}

func (r *Report) add(file string, line, hits int) {
	if file == "" || line <= 0 {
		return
	}
	file = path.Clean(strings.ReplaceAll(strings.TrimSpace(file), "\\", "/"))
	m := r.Files[file]
	if m == nil {
		m = map[int]int{}
		r.Files[file] = m
	}
	if old, ok := m[line]; !ok || hits > old {
		m[line] = hits
	}
}

// Parse detects the format and parses the report.
func Parse(data []byte) (*Report, error) {
	if len(data) > MaxReportBytes {
		return nil, fmt.Errorf("diffcov: report larger than %d MB", MaxReportBytes>>20)
	}
	t := bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	switch {
	case bytes.HasPrefix(t, []byte("mode:")):
		return parseGo(t)
	case bytes.HasPrefix(t, []byte("<")):
		return parseXML(t)
	case bytes.HasPrefix(t, []byte("TN:")) || bytes.HasPrefix(t, []byte("SF:")):
		return parseLCOV(t)
	}
	return nil, errors.New("diffcov: unknown report format (want Cobertura/Clover XML, Go cover profile or LCOV)")
}

// ---------------------------------------------------------------------------
// XML: Cobertura and Clover share the root element <coverage>.
// ---------------------------------------------------------------------------

type xmlLine struct {
	Number int    `xml:"number,attr"` // cobertura
	Hits   string `xml:"hits,attr"`   // cobertura
	Num    int    `xml:"num,attr"`    // clover
	Count  string `xml:"count,attr"`  // clover
}

func parseXML(data []byte) (*Report, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	// Never resolve external entities: Go's decoder does not, and DTDs are
	// skipped; there is nothing to fetch.
	rep := &Report{Files: map[string]map[int]int{}}
	var sources []string
	var curFile string // current <class filename> (cobertura) or <file name> (clover)
	var curKind string // element that set curFile: "class" or "file"
	var inSource bool
	var srcText strings.Builder
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("diffcov: xml: %w", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			switch el.Name.Local {
			case "project":
				if rep.Format == "" {
					rep.Format = FormatClover
				}
			case "packages", "sources":
				if rep.Format == "" {
					rep.Format = FormatCobertura
				}
			case "source":
				inSource = true
				srcText.Reset()
			case "class":
				if f := attr(el, "filename"); f != "" {
					rep.Format = FormatCobertura
					curFile, curKind = f, "class"
				}
			case "file":
				if f := cloverFile(el); f != "" {
					if rep.Format == "" {
						rep.Format = FormatClover
					}
					curFile, curKind = f, "file"
				}
			case "line":
				if curFile == "" {
					continue
				}
				var l xmlLine
				if err := dec.DecodeElement(&l, &el); err != nil {
					return nil, fmt.Errorf("diffcov: xml line: %w", err)
				}
				if l.Number > 0 {
					rep.add(curFile, l.Number, atoi(l.Hits))
				} else if l.Num > 0 {
					rep.add(curFile, l.Num, atoi(l.Count))
				}
			}
		case xml.CharData:
			if inSource {
				srcText.Write(el)
			}
		case xml.EndElement:
			switch el.Name.Local {
			case "source":
				inSource = false
				if s := strings.TrimSpace(srcText.String()); s != "" {
					sources = append(sources, s)
				}
			case "class", "file":
				// Clover nests <class> (without filename) inside <file>:
				// only the element that set the file ends it.
				if el.Name.Local == curKind {
					curFile, curKind = "", ""
				}
			}
		}
	}
	if rep.Format == "" {
		return nil, errors.New("diffcov: xml is neither Cobertura nor Clover")
	}
	// Cobertura filenames are relative to a <source>; keep the joined path
	// when there is exactly one source (the usual case), so suffix matching
	// sees the full path.
	if rep.Format == FormatCobertura && len(sources) == 1 && !strings.HasPrefix(sources[0], ".") {
		joined := map[string]map[int]int{}
		for f, lines := range rep.Files {
			if !strings.HasPrefix(f, "/") {
				f = path.Join(strings.ReplaceAll(sources[0], "\\", "/"), f)
			}
			joined[f] = lines
		}
		rep.Files = joined
	}
	return rep, nil
}

// cloverFile is the path of a Clover <file> element ("" without a name).
// name may be a base name only; path is then the file's full path
// (istanbul/Jest, OpenClover) or, defensively, its directory.
func cloverFile(el xml.StartElement) string {
	n := attr(el, "name")
	if n == "" || strings.Contains(n, "/") {
		return n
	}
	p := strings.TrimRight(strings.ReplaceAll(attr(el, "path"), "\\", "/"), "/")
	switch {
	case p == "":
		return n
	case path.Base(p) == n:
		return p
	default:
		return p + "/" + n
	}
}

func attr(el xml.StartElement, name string) string {
	for _, a := range el.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// atoi parses a hit count or line number; "2.0" counts too, garbage is 0.
func atoi(s string) int {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int(f)
	}
	return 0
}

// ---------------------------------------------------------------------------
// Go cover profile
// ---------------------------------------------------------------------------

// parseGo reads "mode: set|count|atomic" followed by
// "file.go:startLine.startCol,endLine.endCol numStmt count" lines. Every
// line of a block is executable; its hits are the max count of the blocks
// covering it.
func parseGo(data []byte) (*Report, error) {
	rep := &Report{Format: FormatGo, Files: map[string]map[int]int{}}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "mode:") {
			continue
		}
		colon := strings.LastIndex(l, ":")
		if colon < 0 {
			return nil, fmt.Errorf("diffcov: go profile line %q", l)
		}
		file, rest := l[:colon], strings.Fields(l[colon+1:])
		if len(rest) != 3 {
			return nil, fmt.Errorf("diffcov: go profile line %q", l)
		}
		start, end, ok := goRange(rest[0])
		if !ok {
			return nil, fmt.Errorf("diffcov: go profile range %q", rest[0])
		}
		count := atoi(rest[2])
		for ln := start; ln <= end && ln-start < 100000; ln++ {
			rep.add(file, ln, count)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("diffcov: go profile: %w", err)
	}
	return rep, nil
}

// goRange parses "12.3,15.2".
func goRange(s string) (int, int, bool) {
	a, b, ok := strings.Cut(s, ",")
	if !ok {
		return 0, 0, false
	}
	sl, _, _ := strings.Cut(a, ".")
	el, _, _ := strings.Cut(b, ".")
	start, err1 := strconv.Atoi(sl)
	end, err2 := strconv.Atoi(el)
	if err1 != nil || err2 != nil || start <= 0 || end < start {
		return 0, 0, false
	}
	return start, end, true
}

// ---------------------------------------------------------------------------
// LCOV
// ---------------------------------------------------------------------------

func parseLCOV(data []byte) (*Report, error) {
	rep := &Report{Format: FormatLCOV, Files: map[string]map[int]int{}}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var cur string
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(l, "SF:"):
			cur = strings.TrimPrefix(l, "SF:")
		case strings.HasPrefix(l, "DA:") && cur != "":
			parts := strings.Split(strings.TrimPrefix(l, "DA:"), ",")
			if len(parts) >= 2 {
				rep.add(cur, atoi(parts[0]), atoi(parts[1]))
			}
		case l == "end_of_record":
			cur = ""
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("diffcov: lcov: %w", err)
	}
	return rep, nil
}
