package verify

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

func TestProvenBy(t *testing.T) {
	mg := map[string]bool{finding.ID("db/m.sql", "Удаление колонки"): true}
	lr := &linters.Run{Res: &lint.Result{Findings: []lint.Finding{
		{Tool: lint.ToolGitleaks, Path: "app/cfg.go", Line: 10},
		{Tool: "ci:ruff", Path: "app/x.py", Line: 3},
	}}}
	cr := &compatcheck.Run{Enabled: true, Res: &compat.Result{Changes: []compat.Change{{Source: compat.SourceOasdiff, Path: "api/openapi.yaml", Line: 40, Rule: "r", Message: "removed"}}}}
	p := provenBy(mg, lr, cr)
	for _, tc := range []struct {
		name string
		f    output.Finding
		want string
	}{
		{"migration evidence", output.Finding{Path: "db/m.sql", Line: 3, Title: "Удаление колонки"}, provenMigration},
		{"migration other title", output.Finding{Path: "db/m.sql", Line: 3, Title: "Индекс"}, ""},
		{"secret on the gitleaks line", output.Finding{Path: "app/cfg.go", Line: 9, EndLine: 11, Title: "В коде токен доступа"}, provenGitleaks},
		{"not about a secret", output.Finding{Path: "app/cfg.go", Line: 10, Title: "Гонка"}, ""},
		{"secret elsewhere", output.Finding{Path: "app/cfg.go", Line: 30, Title: "Пароль в логе"}, ""},
		{"ci linter is no proof", output.Finding{Path: "app/x.py", Line: 3, Title: "secret"}, ""},
		{"compat line", output.Finding{Path: "api/openapi.yaml", Line: 42, Title: "Удалено поле"}, provenCompat},
		{"compat far", output.Finding{Path: "api/openapi.yaml", Line: 80, Title: "Удалено поле"}, ""},
		{"compat no line", output.Finding{Path: "api/openapi.yaml", Title: "Удалено поле"}, ""},
	} {
		if got := p(tc.f); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if provenBy(nil, nil, nil) != nil {
		t.Error("nothing to prove with: nil")
	}
}

// A deterministic breaking change proves only a finding about that change:
// the finding speaks of a compatibility break, names what the tool names or
// is about the same kind of change. Any other finding on the same lines (a
// security issue on the spec line, say) goes to the verification.
func TestProvenCompatNeedsTheTopic(t *testing.T) {
	cr := &compatcheck.Run{Enabled: true, Res: &compat.Result{Changes: []compat.Change{
		{Source: compat.SourceOasdiff, Path: "api.yaml", Line: 10, Rule: "response-property-removed",
			Message: "GET /users/{id}: removed the optional property 'email' from the response"},
		{Source: compat.SourceBuf, Path: "a.proto", Line: 5, Rule: "FIELD_SAME_TYPE", Message: `Field "2" on message "User" changed type from "int32" to "string".`},
	}}}
	p := provenBy(nil, nil, cr)
	for _, tc := range []struct {
		f    output.Finding
		want string
	}{
		{output.Finding{Path: "api.yaml", Line: 10, Title: "Нет авторизации на эндпоинте", Body: "любой может читать"}, ""},
		{output.Finding{Path: "api.yaml", Line: 10, Title: "SQL-инъекция в фильтре"}, ""},
		{output.Finding{Path: "api.yaml", Line: 10, Title: "Ломает обратную совместимость"}, provenCompat},
		{output.Finding{Path: "api.yaml", Line: 11, Title: "Breaking change for clients"}, provenCompat},
		{output.Finding{Path: "api.yaml", Line: 9, Title: "Поле email пропало из ответа"}, provenCompat}, // names the property
		{output.Finding{Path: "api.yaml", Line: 10, Title: "Удалено поле из ответа"}, provenCompat},      // same kind: removal
		{output.Finding{Path: "a.proto", Line: 5, Title: "Тип поля изменён"}, provenCompat},              // same kind: type change
		{output.Finding{Path: "a.proto", Line: 5, Title: "Удалено поле"}, ""},                            // another kind
		{output.Finding{Path: "a.proto", Line: 5, Title: "Опечатка в комментарии"}, ""},
		{output.Finding{Path: "api.yaml", Line: 10, Title: "Поле emails и email_raw без лимита"}, ""}, // not the word 'email'
		{output.Finding{Path: "api.yaml", Line: 10, Title: "Лимит для emails; поле email без лимита"}, provenCompat},
	} {
		if got := p(tc.f); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.f, got, tc.want)
		}
	}
}
