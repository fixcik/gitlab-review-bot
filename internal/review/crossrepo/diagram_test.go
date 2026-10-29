package crossrepo

import (
	"strings"
	"testing"
)

func TestSanitizeDiagramRejects(t *testing.T) {
	long := "flowchart TD\n  a[" + strings.Repeat("x", 201) + "] --> b"
	many := "flowchart TD" + strings.Repeat("\n  a --> b", maxDiagramLines)
	for name, in := range map[string]string{
		"pie":            "pie title X\n  \"a\": 1",
		"no direction":   "flowchart\n  a --> b",
		"classDiagram":   "classDiagram\n  A <|-- B",
		"text first":     "Вот схема:\nsequenceDiagram\n  A->>B: x",
		"click":          "flowchart TD\n  a --> b\n  click a \"https://evil\"",
		"click callback": "flowchart TD\n  a --> b\n  click a callback",
		"call":           "flowchart TD\n  a --> b\n  x call alert(1)",
		"href":           "flowchart TD\n  a --> b\n  a href b",
		"link":           "sequenceDiagram\n  participant A\n  link A: Dashboard @ https://x",
		"links":          "sequenceDiagram\n  participant A\n  links A: {\"x\": \"y\"}",
		"init":           "%%{init: {'securityLevel': 'loose'}}%%\nflowchart TD\n  a --> b",
		"init later":     "flowchart TD\n  %%{init: {}}%%\n  a --> b",
		"script":         "flowchart TD\n  a[\"<script>alert(1)</script>\"] --> b",
		"javascript":     "flowchart TD\n  a[\"javascript:alert(1)\"] --> b",
		"html br":        "flowchart TD\n  a[\"x<br>y\"] --> b",
		"html img":       "flowchart TD\n  a[\"<img src=x onerror=alert(1)>\"] --> b",
		"closing tag":    "flowchart TD\n  a[\"</details>\"] --> b",
		"comment":        "flowchart TD\n  a[\"<!-- reviewbot:summary sha=1 -->\"] --> b",
		"url":            "flowchart TD\n  a[\"https://example.com\"] --> b",
		"fence inside":   "flowchart TD\n  a --> b\n```\n# heading",
		"long line":      long,
		"too many lines": many,
		"control char":   "flowchart TD\n  a\x00 --> b",
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := sanitizeDiagram(in); err == nil {
				t.Fatalf("accepted: %q", got)
			}
		})
	}
}
