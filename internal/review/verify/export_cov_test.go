package verify

import (
	"testing"
	"text/template"
)

// BreakTemplate replaces the prompt template *p by bad until the test ends
// (render failures).
func BreakTemplate(t *testing.T, p **template.Template, bad *template.Template) {
	t.Helper()
	old := *p
	*p = bad
	t.Cleanup(func() { *p = old })
}

// VerifyUserTmpl is the findings run's user prompt template.
var VerifyUserTmpl = &verifyUserTmpl
