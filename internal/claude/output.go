package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// cliResult is the result object: the final `result` event of `claude -p
// --output-format stream-json --verbose` (same fields as `--output-format json`).
type cliResult struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	NumTurns         int             `json:"num_turns"`
	DurationMS       int64           `json:"duration_ms"`
	Errors           []string        `json:"errors"`
	// APIErrorStatus is the HTTP status of the API error that ended the run
	// (0 if none).
	APIErrorStatus int `json:"api_error_status"`
	// Token usage (see usage.go).
	Usage      *cliUsage                `json:"usage"`
	ModelUsage map[string]cliModelUsage `json:"modelUsage"`
}

// processOutput is what a finished claude process left behind.
type processOutput struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int   // 0 on success
	Err      error // exec error (non-nil with ExitCode != 0, or failure to start)
}

const tailBytes = 2000

// parseOutput turns the CLI output into a RunResult or an error.
// wantJSON is true when a JSON schema was requested.
func parseOutput(p processOutput, wantJSON bool) (*core.RunResult, error) {
	res, perr := decodeResult(p.Stdout)
	if perr != nil {
		// No result object: look for rate-limit signs in what the CLI itself
		// printed (see cliMessages), then report.
		if rl := detectRateLimit(cliMessages(p.Stdout) + "\n" + string(p.Stderr)); rl != nil {
			return nil, rl
		}
		if p.ExitCode != 0 || p.Err != nil {
			return nil, fmt.Errorf("claude exited with code %d: %s", p.ExitCode, describeFailure(p))
		}
		return nil, fmt.Errorf("claude: unexpected output (%v): %s", perr, tail(string(p.Stdout), tailBytes))
	}

	if res.IsError || (res.Subtype != "" && res.Subtype != "success") {
		msg := strings.TrimSpace(res.Result)
		if msg == "" && len(res.Errors) > 0 {
			msg = strings.Join(res.Errors, "; ")
		}
		if msg == "" {
			msg = res.Subtype
		}
		if res.APIErrorStatus == http.StatusUnauthorized {
			return nil, withUsage(res, &core.ErrClaudeAuth{Message: tail(msg, 500)})
		}
		if rl := detectRateLimit(msg + "\n" + string(p.Stderr)); rl != nil {
			return nil, withUsage(res, rl)
		}
		return nil, withUsage(res, fmt.Errorf("claude run failed (%s): %s", nonEmpty(res.Subtype, "error"), tail(msg, tailBytes)))
	}

	m := res.metrics()
	out := &m
	if so := bytes.TrimSpace(res.StructuredOutput); len(so) > 0 && !bytes.Equal(so, []byte("null")) {
		// so is valid JSON (decodeResult unmarshalled it into a RawMessage):
		// re-encode it compactly with sorted keys.
		var v any
		_ = json.Unmarshal(so, &v)
		b, _ := json.Marshal(v)
		out.Output = string(b)
		return out, nil
	}
	if wantJSON {
		if j, ok := extractJSON(res.Result); ok {
			out.Output = j
			return out, nil
		}
		// The text of a successful result is the model's own answer: a
		// prompt injection can make it say "usage limit reached|<epoch>",
		// so it is never searched for rate-limit signs (that would postpone
		// the review at will). A real limit arrives as an error result, a
		// rejected rate_limit_event (streamCapture.refineError) or on stderr.
		return nil, withUsage(res, fmt.Errorf("claude: no JSON object in result: %s", tail(res.Result, tailBytes)))
	}
	out.Output = res.Result
	return out, nil
}

// cliMessages is the part of stdout without a result object that the CLI
// itself wrote: plain-text lines (its own messages, and the lines
// streamCapture synthesizes from API-error and rejected rate-limit events).
// Lines that look like JSON are skipped: a truncated or unparsed event (a
// process killed mid-line, a JSON array of messages) can carry the model's
// text.
func cliMessages(stdout []byte) string {
	var b strings.Builder
	for _, l := range strings.Split(string(stdout), "\n") {
		if l = strings.TrimSpace(l); l == "" || l[0] == '{' || l[0] == '[' {
			continue
		}
		b.WriteString(l + "\n")
	}
	return b.String()
}

// decodeResult finds the result object in stdout. It tolerates leading
// noise and, defensively, stream-json style output (last "result" line).
func decodeResult(stdout []byte) (*cliResult, error) {
	s := bytes.TrimSpace(stdout)
	if len(s) == 0 {
		return nil, errors.New("empty output")
	}
	var r cliResult
	if err := json.Unmarshal(s, &r); err == nil && (r.Type != "" || r.Result != "" || len(r.StructuredOutput) > 0) {
		return &r, nil
	}
	// One JSON value per line: take the last "result".
	lines := bytes.Split(s, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		l := bytes.TrimSpace(lines[i])
		if len(l) == 0 || l[0] != '{' {
			continue
		}
		var lr cliResult
		if err := json.Unmarshal(l, &lr); err == nil && lr.Type == "result" {
			return &lr, nil
		}
	}
	// A JSON array of messages (some versions with --verbose).
	var arr []json.RawMessage
	if err := json.Unmarshal(s, &arr); err == nil {
		for i := len(arr) - 1; i >= 0; i-- {
			var lr cliResult
			if err := json.Unmarshal(arr[i], &lr); err == nil && lr.Type == "result" {
				return &lr, nil
			}
		}
	}
	return nil, errors.New("not a claude JSON result")
}

var fenceRe = regexp.MustCompile("(?s)```(?:json|JSON)?[ \t]*\r?\n(.*?)\r?\n?```")

// extractJSON returns a JSON object found in text: a fenced ```json block,
// the whole text, or the first balanced {...} that parses.
func extractJSON(text string) (string, bool) {
	text = strings.TrimSpace(text)
	for _, m := range fenceRe.FindAllStringSubmatch(text, -1) {
		if j, ok := compactObject(m[1]); ok {
			return j, true
		}
	}
	if j, ok := compactObject(text); ok {
		return j, true
	}
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		var v map[string]any
		if err := dec.Decode(&v); err == nil {
			b, _ := json.Marshal(v)
			return string(b), true
		}
	}
	return "", false
}

func compactObject(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return "", false
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return "", false
	}
	b, _ := json.Marshal(v)
	return string(b), true
}

var (
	// "Claude AI usage limit reached|1735689600"
	limitTSRe   = regexp.MustCompile(`(?i)limit[^|\n]*\|\s*(\d{10,13})`)
	rateLimitRe = regexp.MustCompile(`(?i)usage limit|rate[ _-]?limit|too many requests|overloaded|\b429\b|\b529\b|limit reached|out of (extra )?usage|credit balance is too low`)
	// "resets 5pm", "try again at 1735689600"
	trailingTSRe = regexp.MustCompile(`(?:\||\bat\s|reset[s]?\s(?:at\s)?)\s*(\d{10,13})\b`)
)

// detectRateLimit returns *core.ErrClaudeRateLimited if text looks like a
// usage/rate limit message.
func detectRateLimit(text string) *core.ErrClaudeRateLimited {
	msg, ok := firstMatchingLine(text)
	if !ok {
		return nil
	}
	e := &core.ErrClaudeRateLimited{Message: msg}
	ts := ""
	if m := limitTSRe.FindStringSubmatch(text); m != nil {
		ts = m[1]
	} else if m := trailingTSRe.FindStringSubmatch(text); m != nil {
		ts = m[1]
	}
	if ts != "" {
		if n, err := strconv.ParseInt(ts, 10, 64); err == nil {
			if len(ts) == 13 {
				e.ResetAt = time.UnixMilli(n)
			} else {
				e.ResetAt = time.Unix(n, 0)
			}
		}
	}
	return e
}

// firstMatchingLine is the first line of text that looks like a rate-limit
// message. No rateLimitRe alternative spans a newline, so text matches iff
// one of its lines does.
func firstMatchingLine(text string) (string, bool) {
	for _, l := range strings.Split(text, "\n") {
		if rateLimitRe.MatchString(l) {
			return tail(strings.TrimSpace(l), 500), true
		}
	}
	return "", false
}

func describeFailure(p processOutput) string {
	var parts []string
	if s := strings.TrimSpace(string(p.Stderr)); s != "" {
		parts = append(parts, "stderr: "+tail(s, tailBytes))
	}
	if s := strings.TrimSpace(string(p.Stdout)); s != "" {
		parts = append(parts, "stdout: "+tail(s, tailBytes/2))
	}
	if len(parts) == 0 && p.Err != nil {
		parts = append(parts, p.Err.Error())
	}
	return strings.Join(parts, "; ")
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
