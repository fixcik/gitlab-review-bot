package learn

import (
	"strings"
	"testing"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		in    string
		ok    bool
		kind  CommandKind
		id    int64
		text  string
		scope string
	}{
		{"правила", true, CmdList, 0, "", ""},
		{"Rules", true, CmdList, 0, "", ""},
		{"правила.", true, CmdList, 0, "", ""},
		{"забудь #12", true, CmdForget, 12, "", ""},
		{"забудь 12", true, CmdForget, 12, "", ""},
		{"forget #3", true, CmdForget, 3, "", ""},
		{"забудь правило #7", true, CmdForget, 7, "", ""},
		{"правило #12 для всех", true, CmdScope, 12, "", ScopeGlobal},
		{"правило #12 только здесь", true, CmdScope, 12, "", ScopeProject},
		{"rule #5 global", true, CmdScope, 5, "", ScopeGlobal},
		{"rule 5 local", true, CmdScope, 5, "", ScopeProject},
		{"правило #12 зачем?", false, 0, 0, "", ""},
		{"запомни не требуй ретраев для API-шлюза", true, CmdRemember, 0, "не требуй ретраев для API-шлюза", ""},
		{"запомни: логи на английском", true, CmdRemember, 0, "логи на английском", ""},
		{"remember use errors.Is", true, CmdRemember, 0, "use errors.Is", ""},
		{"запомни для всех: логи на английском", true, CmdRemember, 0, "логи на английском", ScopeGlobal},
		{"запомни логи на английском (только здесь)", true, CmdRemember, 0, "логи на английском", ScopeProject},
		{"запомни логи на английском, для всех", true, CmdRemember, 0, "логи на английском", ScopeGlobal},
		{"запомни многострочное\nправило", true, CmdRemember, 0, "многострочное\nправило", ""},
		{"запомни", false, 0, 0, "", ""},
		{"почему тут мьютекс?", false, 0, 0, "", ""},
		{"забудь про это", false, 0, 0, "", ""},
		{"правила\nи ещё вопрос", false, 0, 0, "", ""},
	}
	for _, tt := range tests {
		c, ok := ParseCommand(tt.in)
		if ok != tt.ok {
			t.Errorf("%q: ok=%v, want %v (%+v)", tt.in, ok, tt.ok, c)
			continue
		}
		if !ok {
			continue
		}
		if c.Kind != tt.kind || c.ID != tt.id || c.Text != tt.text || c.Scope != tt.scope {
			t.Errorf("%q: got %+v, want kind=%d id=%d text=%q scope=%q", tt.in, c, tt.kind, tt.id, tt.text, tt.scope)
		}
	}
}

func TestScrubAndClean(t *testing.T) {
	for _, s := range []string{
		"https://user:p4ssw0rd@gitlab.example.com/repo.git",
		"glpat-" + "AAAAbbbbCCCCdddd1234",
		"sk-ant-api03-abcdefghijklmnop",
		"token=abcdef123456",
		"Bearer abcdefghijklmnopqrstuvwxyz",
		"ghp_abcdefghijklmnopqrstuvwxyz0123",
		"AKIAABCDEFGHIJKLMNOP",
		strings.Repeat("a1", 25),
	} {
		got := Scrub("ключ " + s + " конец")
		if strings.Contains(got, s) || !strings.Contains(got, Redacted) {
			t.Errorf("not scrubbed: %q -> %q", s, got)
		}
	}
	keep := "Не требуй ретраев в internal/client/http_client.go: ретраи делает API-шлюз (https://wiki.example.com/gw)."
	if got := Scrub(keep); got != keep {
		t.Errorf("plain text changed: %q", got)
	}
	if got := CleanRule("  <!-- reviewbot:finding id=abc -->Правило\n с  переносом  "); got != "Правило с переносом" {
		t.Errorf("clean: %q", got)
	}
	if got := CleanRule("glpat-" + "AAAAbbbbCCCCdddd1234"); got != "" {
		t.Errorf("only a secret must give empty rule: %q", got)
	}
	long := CleanRule(strings.Repeat("слово ", 200))
	if n := len([]rune(long)); n > MaxRuleRunes+1 || !strings.HasSuffix(long, "…") {
		t.Errorf("cap: %d %q", n, long[len(long)-10:])
	}
}

func TestNormalize(t *testing.T) {
	if NormalizeKey("  Не   требуй Ретраев. ") != NormalizeKey("не требуй ретраев") {
		t.Fatal("keys differ")
	}
	if NormalizeScope("GLOBAL") != ScopeGlobal || NormalizeScope("whatever") != ScopeProject || NormalizeScope("") != ScopeProject {
		t.Fatal("scope")
	}
	if !(Rule{Scope: ScopeGlobal, ProjectPath: "a"}).AppliesTo("b") || (Rule{Scope: ScopeProject, ProjectPath: "a"}).AppliesTo("b") || !(Rule{Scope: ScopeProject, ProjectPath: "A"}).AppliesTo("a") {
		t.Fatal("applies")
	}
}

func TestSelect(t *testing.T) {
	rules := []Rule{{ID: 1, Text: "aaaa"}, {ID: 2, Text: "bbbb"}, {ID: 3, Text: "cccc"}}
	if got := Select(rules, 2, 0); len(got) != 2 || got[1].ID != 2 {
		t.Fatalf("count cap: %+v", got)
	}
	if got := Select(rules, 0, 9); len(got) != 2 {
		t.Fatalf("char cap: %+v", got)
	}
	if got := Select(rules, 0, 0); len(got) != 3 {
		t.Fatalf("no cap: %+v", got)
	}
}
