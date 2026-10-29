package sysmap

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Field caps (runes) for strings coming from repositories or the model.
const (
	maxPathRunes  = 300
	maxEventRunes = 120
	maxRawRunes   = 300
	maxBaseRunes  = 160
	maxNoteRunes  = 200
	// maxServiceRunes caps Entry.ExternalService.
	maxServiceRunes = 60
	// maxHostLen is the longest DNS name.
	maxHostLen = 253
)

var httpMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
	"HEAD": true, "OPTIONS": true, "TRACE": true,
}

// NormalizeMethod returns the upper-case HTTP method, "" when unknown or
// "any" (ANY, ALL, *, MATCH, ROUTE, ...).
func NormalizeMethod(m string) string {
	m = strings.ToUpper(strings.TrimSpace(m))
	if httpMethods[m] {
		return m
	}
	return ""
}

var (
	// Django: (?P<id>[^/]+) -> {id}; other regex groups -> {}.
	namedGroupRe = regexp.MustCompile(`\(\?P<([A-Za-z_][A-Za-z0-9_]*)>[^)]*\)`)
	groupRe      = regexp.MustCompile(`\([^)]*\)`)
	// Python %(name)s before regex groups are rewritten.
	printfNamedRe = regexp.MustCompile(`%\(([A-Za-z_][A-Za-z0-9_]*)\)[-+ #0-9.]*[sdvfqxXu]`)
	// placeholders inside one segment, in the order they are tried.
	dollarRe   = regexp.MustCompile(`\$\{\s*([^}]*)\}`)            // ${id}, ${order.id}
	mustacheRe = regexp.MustCompile(`\{\{\s*([^}]*)\}\}`)          // {{id}}
	braceRe    = regexp.MustCompile(`\{([^{}]*)\}`)                // {id}, {order_id}, {0}, {id:int}
	angleRe    = regexp.MustCompile(`<(?:[A-Za-z_]+:)?([^<>]*)>`)  // <int:id>, <id>
	bracketRe  = regexp.MustCompile(`^\[(?:\.\.\.)?([^\[\]]*)\]$`) // Next.js [id], [...slug]
	printfRe   = regexp.MustCompile(`%(?:\(([A-Za-z_][A-Za-z0-9_]*)\))?[-+ #0-9.]*[sdvfqxXu]`)
	colonRe    = regexp.MustCompile(`^:([A-Za-z_][A-Za-z0-9_]*)`)
	identRe    = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	segCharsRe = regexp.MustCompile(`^[A-Za-z0-9_.~{}\-@]+$`)
	versionRe  = regexp.MustCompile(`^v[0-9]+$`)
	eventRe    = regexp.MustCompile(`^[A-Za-z0-9_.:/\-*]+$`)
)

// paramName turns a placeholder expression into a short parameter name
// ("order.id" -> "id", "" -> "").
func paramName(expr string) string {
	ids := identRe.FindAllString(expr, -1)
	if len(ids) == 0 {
		return ""
	}
	n := ids[len(ids)-1]
	if len(n) > 40 {
		n = n[:40]
	}
	return n
}

func brace(name string) string { return "{" + name + "}" }

// normalizeSegment rewrites the placeholders of one path segment to {name}
// ({} when unnamed). A segment that still holds characters outside a
// conservative URL set (string concatenation leftovers, quotes, spaces)
// becomes {}.
func normalizeSegment(s string) string {
	if m := bracketRe.FindStringSubmatch(s); m != nil {
		return brace(paramName(m[1]))
	}
	if m := colonRe.FindStringSubmatch(s); m != nil && len(m[0]) == len(s) {
		return brace(m[1])
	}
	if s == "*" || s == "**" {
		return "{}"
	}
	s = dollarRe.ReplaceAllStringFunc(s, func(x string) string { return brace(paramName(dollarRe.FindStringSubmatch(x)[1])) })
	s = mustacheRe.ReplaceAllStringFunc(s, func(x string) string { return brace(paramName(mustacheRe.FindStringSubmatch(x)[1])) })
	s = braceRe.ReplaceAllStringFunc(s, func(x string) string {
		inner := braceRe.FindStringSubmatch(x)[1]
		if i := strings.IndexAny(inner, ":="); i >= 0 { // {id:int}, {id=*}
			inner = inner[:i]
		}
		if strings.Trim(inner, "0123456789") == "" { // {0}, {1}
			return "{}"
		}
		return brace(paramName(inner))
	})
	s = angleRe.ReplaceAllStringFunc(s, func(x string) string { return brace(paramName(angleRe.FindStringSubmatch(x)[1])) })
	s = printfRe.ReplaceAllStringFunc(s, func(x string) string {
		m := printfRe.FindStringSubmatch(x)
		return brace(m[1])
	})
	if !segCharsRe.MatchString(s) {
		return "{}"
	}
	return s
}

// NormalizePath returns the normalized path template of a URL or path as
// written in code or a spec: scheme, host and any base expression before
// the first "/" are dropped, the query string and fragment are dropped,
// placeholders become {name} ({} when unnamed), the trailing slash is
// removed. "" when nothing path-like is left.
func NormalizePath(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "\"'`")
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:]
		j := strings.IndexByte(rest, '/')
		if j < 0 {
			return "/"
		}
		s = rest[j:]
	}
	s = strings.TrimPrefix(s, "^")
	s = printfNamedRe.ReplaceAllString(s, "{$1}")
	s = namedGroupRe.ReplaceAllString(s, "{$1}")
	s = groupRe.ReplaceAllString(s, "{}")
	s = strings.TrimSuffix(s, "$")
	s = strings.TrimSuffix(s, "/?")
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	if !strings.HasPrefix(s, "/") {
		// A base expression or host before the path (`${BASE}/orders`,
		// `ORDERS_URL/x`); a bare relative path ("orders/1") is kept.
		if i := strings.IndexByte(s, '/'); i >= 0 && looksLikeBase(s[:i]) {
			s = s[i:]
		} else {
			s = "/" + s
		}
	}
	var segs []string
	for _, seg := range strings.Split(s, "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "." {
			continue
		}
		segs = append(segs, normalizeSegment(seg))
	}
	out := "/" + strings.Join(segs, "/")
	if utf8.RuneCountInString(out) > maxPathRunes {
		return ""
	}
	return out
}

// looksLikeBase reports a leading segment that is a host or a base URL
// expression rather than a path segment.
func looksLikeBase(h string) bool {
	if strings.ContainsAny(h, "${}%.:+ ()") {
		return true
	}
	return strings.Contains(h, "_") && strings.ToUpper(h) == h
}

// PathKey is the matching key of a normalized path: lower case, every
// segment holding a placeholder becomes {}.
func PathKey(p string) string {
	if p == "" {
		return ""
	}
	segs := strings.Split(strings.Trim(strings.ToLower(p), "/"), "/")
	for i, s := range segs {
		if strings.Contains(s, "{") {
			segs[i] = "{}"
		}
	}
	k := "/" + strings.Join(segs, "/")
	if k == "//" {
		return "/"
	}
	return k
}

// StripPrefix drops a common API prefix from a path key: /api, /api/vN,
// /vN. It returns the key unchanged when there is none.
func StripPrefix(key string) string {
	segs := strings.Split(strings.Trim(key, "/"), "/")
	i := 0
	if i < len(segs) && segs[i] == "api" {
		i++
	}
	if i < len(segs) && versionRe.MatchString(segs[i]) {
		i++
	}
	if i == 0 {
		return key
	}
	return "/" + strings.Join(segs[i:], "/")
}

// genericKey reports a path too unspecific to match on: "/" or only
// placeholders ("/{}", "/{}/{}").
func genericKey(key string) bool {
	for _, s := range strings.Split(strings.Trim(key, "/"), "/") {
		if s != "" && s != "{}" {
			return false
		}
	}
	return true
}

// NormalizeEvent cleans an event name ("" when it is not a plausible
// identifier).
func NormalizeEvent(e string) string {
	e = strings.Trim(strings.TrimSpace(e), "\"'`")
	if e == "" || utf8.RuneCountInString(e) > maxEventRunes || !eventRe.MatchString(e) {
		return ""
	}
	return e
}

var (
	// hostRe: a dotted DNS name of letters, digits and inner hyphens with an
	// alphabetic (or punycode) top-level label.
	hostRe = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+(?:[a-z]{2,63}|xn--[a-z0-9-]{1,59})$`)
	// urlHostRe: a literal http(s)/ws(s) URL in a call expression.
	urlHostRe = regexp.MustCompile(`(?i)\b(?:https?|wss?)://([^\s/?#'"` + "`" + `]+)`)
)

// privateSuffixes are DNS suffixes of hosts that are never a third-party
// system: cluster, local network and reserved names.
var privateSuffixes = []string{".local", ".internal", ".svc", ".lan", ".localhost", ".localdomain", ".home.arpa",
	".test", ".example", ".invalid", ".intranet", ".corp", ".home"}

// NormalizeHost returns the lower-case host name of a host or URL
// ("https://user@OAuth.Yandex.ru:443/token" -> "oauth.yandex.ru"): scheme,
// user info, port, path, query and the trailing dot are dropped. It is ""
// unless the result is a dotted public DNS name: IP addresses, localhost,
// single-label names (docker / k8s service names), cluster and private
// suffixes (.svc, .cluster.local, .internal, .local, ...) and template
// leftovers are rejected.
func NormalizeHost(s string) string {
	s = strings.ToLower(strings.Trim(strings.TrimSpace(s), "\"'`"))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, ".")
	if len(s) > maxHostLen || !hostRe.MatchString(s) {
		return ""
	}
	for _, suf := range privateSuffixes {
		if strings.HasSuffix(s, suf) {
			return ""
		}
	}
	return s
}

// urlHost returns the first public host (NormalizeHost) of a literal
// http(s)/ws(s) URL written in raw or, failing that, in base; "" when there
// is none. A bare base without a scheme ("settings.api.url") is not
// trusted: it is as often a config key as a host.
func urlHost(raw, base string) string {
	for _, s := range []string{raw, base} {
		for _, m := range urlHostRe.FindAllStringSubmatch(s, -1) {
			if h := NormalizeHost(m[1]); h != "" {
				return h
			}
		}
	}
	return ""
}

// OwnDomain returns the organization's domain from the GitLab URL: the
// GitLab host without its first label ("https://gitlab.example.com" ->
// "example.com"), the host itself when it has two labels
// ("gitlab.com"), "" for a non-public host (IP, localhost, ...).
func OwnDomain(gitlabURL string) string {
	h := NormalizeHost(gitlabURL)
	if strings.Count(h, ".") >= 2 {
		return h[strings.IndexByte(h, '.')+1:]
	}
	return h
}

// underDomain reports whether host is domain or one of its subdomains.
func underDomain(host, domain string) bool {
	return domain != "" && (host == domain || strings.HasSuffix(host, "."+domain))
}

// EventKey is the matching key of an event name: lower case, the
// separators ".", "_", "-" and spaces are one ("chat.update" = "chat_update"
// = "Chat-Update" -> "chat.update"). Other characters (":", "/") stay.
func EventKey(e string) string {
	return strings.Join(eventSegments(e), ".")
}

func eventSegments(e string) []string {
	return strings.FieldsFunc(strings.ToLower(e), func(r rune) bool { return r == '.' || r == '_' || r == '-' || r == ' ' })
}

// sameEvent reports two non-empty event names with the same EventKey.
func sameEvent(a, b string) bool {
	return a != "" && b != "" && EventKey(a) == EventKey(b)
}

// eventSuffix reports whether the shorter of two different event names is
// a whole-segment suffix of the longer ("usage" / "llm.usage").
func eventSuffix(a, b string) bool {
	sa, sb := eventSegments(a), eventSegments(b)
	if len(sa) > len(sb) {
		sa, sb = sb, sa
	}
	if len(sa) == 0 || len(sa) == len(sb) {
		return false
	}
	return strings.Join(sb[len(sb)-len(sa):], ".") == strings.Join(sa, ".")
}

// clip shortens s to n runes on one line.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(strings.ToValidUTF8(s, "")), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
