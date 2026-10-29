package memory

import (
	"strings"
	"testing"
)

func TestParseSection(t *testing.T) {
	cases := map[string]string{
		"test": SectionTest, "Тесты": SectionTest, "тесты:": SectionTest, "ЛИНТ": SectionLint, "сборка": SectionBuild,
		"подготовка окружения": SectionSetup, "Структура проекта": SectionStructure, "pitfalls": SectionPitfalls,
		"подводные камни": SectionPitfalls, "прочее": SectionOther, "other": SectionOther, "unknown": "", "": "",
	}
	for in, want := range cases {
		if got := ParseSection(in); got != want {
			t.Errorf("ParseSection(%q) = %q, want %q", in, got, want)
		}
	}
	for _, k := range Sections {
		if Title(k) == "" || ParseSection(k) != k || ParseSection(Title(k)) != k {
			t.Errorf("section %q: title %q does not round-trip", k, Title(k))
		}
	}
}

func TestCleanBulletScrubsSecrets(t *testing.T) {
	tok := "glpat-" + strings.Repeat("A1b2", 6)
	got := CleanBullet("- Тесты: `GITLAB_TOKEN=" + tok + " make test` <!-- hidden -->\n  через   прокси https://user:pass@nexus.local/repo")
	if strings.Contains(got, tok) || strings.Contains(got, "user:pass") || strings.Contains(got, "hidden") {
		t.Fatalf("secret kept: %q", got)
	}
	if strings.HasPrefix(got, "-") || strings.Contains(got, "\n") || !strings.Contains(got, "make test") {
		t.Fatalf("bad cleanup: %q", got)
	}
	if CleanBullet("  ") != "" || CleanBullet("sk-ant-"+strings.Repeat("x", 30)) != "" {
		t.Fatal("empty / secret-only bullets must be dropped")
	}
	long := CleanBullet(strings.Repeat("слово ", 200))
	if n := len([]rune(long)); n > MaxBulletRunes+1 || !strings.HasSuffix(long, "…") {
		t.Fatalf("not capped: %d", n)
	}
}

func TestBulletsRenderRoundTrip(t *testing.T) {
	b := []string{"`make test-unit` — юнит-тесты без docker", "интеграционные: `make test-int`, нужен docker"}
	if got := Bullets(Render(b)); strings.Join(got, "|") != strings.Join(b, "|") {
		t.Fatalf("round trip %q", got)
	}
	if Render(nil) != "" || Size(nil) != 0 {
		t.Fatal("empty")
	}
}

func TestSimilarAndDedupe(t *testing.T) {
	if !Similar("Запускать `go test ./...`", "запускать go test ./...") {
		t.Fatal("same modulo case/backticks")
	}
	if !Similar("юнит-тесты запускать через make test-unit без docker и сети",
		"юнит-тесты запускать через make test-unit без docker и сети!") {
		t.Fatal("near identical")
	}
	if Similar("make test-unit", "make test-int") {
		t.Fatal("different commands must not be similar")
	}
	got := Dedupe([]string{"go test ./...", "make lint", "`go test ./...`"})
	if len(got) != 2 || got[0] != "make lint" || got[1] != "`go test ./...`" {
		t.Fatalf("dedupe keeps the newest: %q", got)
	}
}

func TestApplyOps(t *testing.T) {
	mem := map[string][]string{SectionTest: {"`go test ./...` падает: пакет store требует docker"}}
	ch := Apply(mem, []Update{
		{Section: "test", Op: "add", Text: "Юнит-тесты: `go test ./internal/...` (без docker)", Command: "go test ./internal/..."},
		{Section: "lint", Op: "ADD", Text: "`golangci-lint run ./...` с конфигом .golangci.yml"},
		{Section: "test", Op: "add", Text: "юнит-тесты: go test ./internal/... (без docker)"}, // duplicate -> no change
		{Section: "nonsense", Op: "add", Text: "Генерация: `make gen` перед сборкой"},         // unknown -> other
		{Section: "test", Op: "replace", Replaces: "пакет store требует docker", Text: "Пакет store: `go test -tags=nodocker ./internal/store`"},
		{Section: "lint", Op: "frobnicate", Text: "ignored"},
		{Section: "lint", Op: "remove", Text: "golangci-lint run"},
		{Section: "build", Op: "remove", Text: "nothing to remove here"},
	})
	if len(ch) != 5 {
		t.Fatalf("changes %+v", ch)
	}
	if ch[0].Op != OpAdd || ch[0].Command != "go test ./internal/..." || ch[2].Section != SectionOther || ch[3].Op != OpReplace || ch[4].Op != OpRemove {
		t.Fatalf("changes %+v", ch)
	}
	if got := mem[SectionTest]; len(got) != 2 || !strings.Contains(got[0], "./internal/...") || !strings.Contains(got[1], "nodocker") {
		t.Fatalf("test section %q", got)
	}
	if len(mem[SectionLint]) != 0 {
		t.Fatalf("lint section %q", mem[SectionLint])
	}
	// Re-adding an existing bullet moves it to the end and reports nothing.
	ch = Apply(mem, []Update{{Section: "test", Op: "add", Text: mem[SectionTest][0]}})
	if len(ch) != 0 || !strings.Contains(mem[SectionTest][1], "./internal/...") {
		t.Fatalf("re-confirm: %+v %q", ch, mem[SectionTest])
	}
	// Same key (only punctuation differs): kept as is, not reported.
	if ch = Apply(mem, []Update{{Section: "test", Op: "add", Text: "Юнит-тесты: `go test ./internal/...` (без docker)."}}); len(ch) != 0 {
		t.Fatalf("same key: %+v", ch)
	}
	// Near-identical new wording without "replaces" replaces in place.
	ch = Apply(mem, []Update{{Section: "test", Op: "add", Text: "Юнит-тесты: `go test ./internal/...` (без docker), быстро"}})
	if len(ch) != 1 || ch[0].Op != OpReplace || len(mem[SectionTest]) != 2 || !strings.Contains(mem[SectionTest][1], "быстро") {
		t.Fatalf("similar add: %+v %q", ch, mem[SectionTest])
	}
}

func TestApplyNeverStoresTokens(t *testing.T) {
	tok := "glpat-" + strings.Repeat("Zx9", 8)
	mem := map[string][]string{}
	Apply(mem, []Update{
		{Section: "setup", Op: "add", Text: "Перед тестами экспортировать NPM_TOKEN=" + tok},
		{Section: "setup", Op: "add", Text: tok},
	})
	all := strings.Join(mem[SectionSetup], "\n")
	if strings.Contains(all, tok) || len(mem[SectionSetup]) != 1 {
		t.Fatalf("token stored: %q", mem[SectionSetup])
	}
}

func TestTrimNewestAndFitTotal(t *testing.T) {
	var b []string
	for i := 0; i < 50; i++ {
		b = append(b, strings.Repeat("x", 40)+string(rune('a'+i%26)))
	}
	got := TrimNewest(b, 300)
	if Size(got) > 300 || got[len(got)-1] != b[len(b)-1] || len(got) == 0 {
		t.Fatalf("trim newest: size %d, %d bullets", Size(got), len(got))
	}
	mem := map[string][]string{SectionTest: append([]string(nil), b[:20]...), SectionLint: {"short"}}
	changed := FitTotal(mem, 400)
	total := 0
	for _, k := range Sections {
		total += Size(mem[k])
	}
	if total > 400 || len(changed) != 1 || changed[0] != SectionTest || len(mem[SectionLint]) != 1 {
		t.Fatalf("fit total: %d %v", total, changed)
	}
	if mem[SectionTest][len(mem[SectionTest])-1] != b[19] {
		t.Fatal("newest bullet must survive")
	}
}

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		want Command
	}{
		{"память", true, Command{Kind: CmdShow}},
		{"Memory", true, Command{Kind: CmdShow}},
		{"память проекта", true, Command{Kind: CmdShow}},
		{"память очистить", true, Command{Kind: CmdClear}},
		{"память очистить всё", true, Command{Kind: CmdClear}},
		{"memory clear test", true, Command{Kind: CmdClear, Section: SectionTest}},
		{"память очистить тесты.", true, Command{Kind: CmdClear, Section: SectionTest}},
		{"память очистить погоду", true, Command{Kind: CmdClear, BadSection: "погоду"}},
		{"память запомни линт: `make lint` требует golangci-lint 1.60", true, Command{Kind: CmdAdd, Section: SectionLint, Text: "`make lint` требует golangci-lint 1.60"}},
		{"memory add structure: handlers live in internal/http", true, Command{Kind: CmdAdd, Section: SectionStructure, Text: "handlers live in internal/http"}},
		{"память запомни подготовка окружения: make deps", true, Command{Kind: CmdAdd, Section: SectionSetup, Text: "make deps"}},
		{"память запомни погода: солнечно", true, Command{Kind: CmdAdd, BadSection: "погода", Text: "солнечно"}},
		{"а что в памяти лежит?", false, Command{}},
		{"память у этого кода течёт?", false, Command{}},
		{"запомни логи на английском", false, Command{}},
		{"", false, Command{}},
	}
	for _, c := range cases {
		got, ok := ParseCommand(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseCommand(%q) = %+v, %v; want %+v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
