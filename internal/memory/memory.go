// Package memory holds the pure parts of the per-project operational memory:
// what the bot learned about building, testing and linting ONE repository
// (one GitLab project), so it is not rediscovered on every run. It defines
// the section model, the storage interface (implemented by *store.SQLite),
// normalization and secret scrubbing of bullets, the merge of Claude's
// memory_updates, size caps and the "@bot память" command parser. It depends
// only on the standard library and internal/learn (scrubbing).
//
// Memory is operational guidance for Claude only. It never feeds the tools
// allowlist or the setup commands: the bot merely suggests .reviewbot.yaml
// lines to people.
package memory

import (
	"context"
	"strings"
	"time"
)

// Section keys (fixed set).
const (
	SectionBuild     = "build"
	SectionTest      = "test"
	SectionLint      = "lint"
	SectionSetup     = "setup"
	SectionStructure = "structure"
	SectionPitfalls  = "pitfalls"
	SectionOther     = "other"
)

// Sections lists the section keys in display order.
var Sections = []string{SectionBuild, SectionTest, SectionLint, SectionSetup, SectionStructure, SectionPitfalls, SectionOther}

var titles = map[string]string{
	SectionBuild:     "Сборка",
	SectionTest:      "Тесты",
	SectionLint:      "Линт",
	SectionSetup:     "Подготовка окружения",
	SectionStructure: "Структура проекта",
	SectionPitfalls:  "Подводные камни и неудачные попытки",
	SectionOther:     "Прочее",
}

// Title is the Russian display name of a section key ("" for unknown keys).
func Title(key string) string { return titles[key] }

// CommandSection reports whether commands remembered in a section may be
// suggested for the tools allowlist (Claude ran them itself).
func CommandSection(key string) bool {
	return key == SectionBuild || key == SectionTest || key == SectionLint
}

// Caps.
const (
	// DefaultSectionCap is the per-section size cap in characters.
	DefaultSectionCap = 1500
	// DefaultTotalCap is the total size cap in characters.
	DefaultTotalCap = 6000
	// MaxBulletRunes caps one bullet.
	MaxBulletRunes = 300
	// HardSectionCap is what the store accepts at most per section (a safety
	// net; the merge keeps sections far below it).
	HardSectionCap = 4000
)

// Section is one stored section of a project's memory.
type Section struct {
	ProjectID   int64
	ProjectPath string // for display; updated when the project moves
	Key         string // one of Sections
	Content     string // Markdown bullet list
	UpdatedAt   time.Time
	UpdatedByMR int64  // MR IID of the last change (0 = unknown)
	SourceNote  string // what changed it: "ревью !7 (abc1234)", "вручную @bob", …
}

// Store keeps project memory. *store.SQLite implements it. Memory is keyed
// by the numeric GitLab project ID, so a renamed or moved repository keeps
// it; ProjectPath is only for display.
type Store interface {
	// ListMemory returns the project's non-empty sections (any order).
	ListMemory(ctx context.Context, projectID int64) ([]Section, error)
	// SaveMemorySection inserts or replaces one section; empty Content
	// deletes it. It also refreshes ProjectPath on every row of the project.
	SaveMemorySection(ctx context.Context, s Section) error
	// ClearMemory deletes one section (key) or all of them (key == "") and
	// returns how many sections were removed.
	ClearMemory(ctx context.Context, projectID int64, key string) (int, error)
	// SetMemoryProjectPath updates the display path of the project's rows.
	SetMemoryProjectPath(ctx context.Context, projectID int64, path string) error
}

var sectionAliases = map[string]string{
	"build": SectionBuild, "сборка": SectionBuild, "билд": SectionBuild, "компиляция": SectionBuild,
	"test": SectionTest, "tests": SectionTest, "testing": SectionTest, "тесты": SectionTest, "тест": SectionTest, "тестирование": SectionTest,
	"lint": SectionLint, "linter": SectionLint, "linters": SectionLint, "линт": SectionLint, "линтер": SectionLint, "линтеры": SectionLint, "линтинг": SectionLint,
	"setup": SectionSetup, "environment": SectionSetup, "env": SectionSetup, "подготовка": SectionSetup, "окружение": SectionSetup,
	"подготовка окружения": SectionSetup, "настройка": SectionSetup, "зависимости": SectionSetup,
	"structure": SectionStructure, "layout": SectionStructure, "структура": SectionStructure, "структура проекта": SectionStructure,
	"pitfalls": SectionPitfalls, "gotchas": SectionPitfalls, "failures": SectionPitfalls, "подводные камни": SectionPitfalls,
	"камни": SectionPitfalls, "неудачные попытки": SectionPitfalls, "подводные камни и неудачные попытки": SectionPitfalls, "грабли": SectionPitfalls,
	"other": SectionOther, "misc": SectionOther, "прочее": SectionOther, "другое": SectionOther, "разное": SectionOther,
}

// ParseSection maps a key, a Russian title or an alias to a section key
// ("" when unknown). Case-insensitive.
func ParseSection(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(strings.Trim(strings.TrimSpace(s), "«»\"'`.,:;!?")), " "))
	return sectionAliases[s]
}
