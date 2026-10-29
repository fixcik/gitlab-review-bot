// Package skills loads team skills (Claude Code SKILL.md format) from a
// directory on the bot's host and prepares them for a Claude run.
//
// The skills directory is trusted (curated by whoever runs the bot), unlike
// the repository under review: skills from .claude/skills in an MR are never
// loaded (the CLI runs with --setting-sources user and
// --disable-slash-commands). Instead of the CLI's own skill machinery, which
// would also expose the CLI's built-in skills, the bot does the same
// progressive disclosure itself: the system prompt lists each skill's name
// and description with the path of its SKILL.md, and Claude reads the
// relevant ones with Read from a read-only copy mounted in the sandbox.
//
// Layout (the usual one):
//
//	<dir>/<name>/SKILL.md          frontmatter: name, description
//	<dir>/<name>/<other files>     references, examples, scripts
package skills

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// Limits keep a mistaken copy (a whole repository dropped into the skills
// directory) from bloating every run.
const (
	MaxSkillBytes    = 2 << 20  // all files of one skill
	MaxTotalBytes    = 20 << 20 // all skills together
	MaxFilesPerSkill = 200
	MaxSkills        = 100
	MaxDescription   = 1024
	maxDepth         = 6
)

// nameRe is the Claude Code skill name rule: lowercase letters, digits and
// hyphens, at most 64 characters.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Skill is one loaded skill.
type Skill struct {
	Name        string
	Description string
	// Files maps the slash-separated path inside the skill directory to
	// the content; always contains "SKILL.md".
	Files map[string][]byte
	Size  int64
}

// Load reads every skill under dir. Broken or oversized skills are skipped
// and reported in warnings; err is returned only when dir itself cannot be
// read. Skills are sorted by name.
func Load(dir string) (list []Skill, warnings []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("skills: read %s: %w", dir, err)
	}
	var total int64
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if !e.IsDir() {
			if e.Type()&fs.ModeSymlink != 0 {
				warnings = append(warnings, name+": symlinks are not followed")
			}
			continue
		}
		if len(list) >= MaxSkills {
			warnings = append(warnings, fmt.Sprintf("more than %d skills, the rest are skipped", MaxSkills))
			break
		}
		s, err := loadOne(filepath.Join(dir, name), name)
		if err != nil {
			warnings = append(warnings, name+": "+err.Error())
			continue
		}
		if total+s.Size > MaxTotalBytes {
			warnings = append(warnings, fmt.Sprintf("%s: skipped, all skills together exceed %d MB", name, MaxTotalBytes>>20))
			continue
		}
		total += s.Size
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, warnings, nil
}

func loadOne(dir, dirName string) (Skill, error) {
	if !nameRe.MatchString(dirName) {
		return Skill{}, errors.New("directory name must be lowercase letters, digits and hyphens (max 64)")
	}
	s := Skill{Name: dirName, Files: map[string][]byte{}}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if strings.Count(rel, string(filepath.Separator)) >= maxDepth {
				return fmt.Errorf("nested deeper than %d levels", maxDepth)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks, devices: never copied
		}
		if len(s.Files) >= MaxFilesPerSkill {
			return fmt.Errorf("more than %d files", MaxFilesPerSkill)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if s.Size+info.Size() > MaxSkillBytes {
			return fmt.Errorf("larger than %d MB", MaxSkillBytes>>20)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		s.Size += int64(len(b))
		s.Files[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		return Skill{}, err
	}
	md, ok := s.Files["SKILL.md"]
	if !ok {
		return Skill{}, errors.New("no SKILL.md")
	}
	fm, err := frontmatter(md)
	if err != nil {
		return Skill{}, err
	}
	if fm.Name != "" && fm.Name != dirName {
		return Skill{}, fmt.Errorf("frontmatter name %q differs from the directory name", fm.Name)
	}
	s.Description = strings.Join(strings.Fields(fm.Description), " ")
	switch {
	case s.Description == "":
		return Skill{}, errors.New("SKILL.md frontmatter has no description")
	case len([]rune(s.Description)) > MaxDescription:
		return Skill{}, fmt.Errorf("description longer than %d characters", MaxDescription)
	}
	return s, nil
}

type meta struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// frontmatter parses the YAML block between the leading "---" lines.
func frontmatter(md []byte) (meta, error) {
	md = bytes.TrimPrefix(md, []byte("\xef\xbb\xbf"))
	text := strings.ReplaceAll(string(md), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return meta{}, errors.New("SKILL.md must start with a --- frontmatter block")
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return meta{}, errors.New("SKILL.md frontmatter is not closed with ---")
	}
	block := text[4 : 4+end]
	var m meta
	if err := yaml.Unmarshal([]byte(block), &m); err != nil {
		// Descriptions often contain an unquoted ": " ("Use when: …"),
		// which strict YAML rejects; read such blocks line by line.
		if m, ok := looseFrontmatter(block); ok {
			return m, nil
		}
		return meta{}, fmt.Errorf("SKILL.md frontmatter: %w", err)
	}
	return m, nil
}

// looseFrontmatter reads "key: value" lines; indented lines continue the
// previous value. ok is false when no known key was found.
func looseFrontmatter(block string) (meta, bool) {
	var m meta
	var cur *string
	found := false
	for _, line := range strings.Split(block, "\n") {
		if line != "" && (line[0] == ' ' || line[0] == '\t') {
			if cur != nil {
				*cur += " " + strings.TrimSpace(line)
			}
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		cur = nil
		if !ok {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch strings.TrimSpace(key) {
		case "name":
			m.Name, cur, found = val, &m.Name, true
		case "description":
			m.Description, cur, found = val, &m.Description, true
		}
	}
	m.Name = strings.TrimSpace(m.Name)
	return m, found
}

// Select drops disabled skills (names compared case-insensitively).
func Select(all []Skill, disable []string) []Skill {
	if len(disable) == 0 {
		return all
	}
	off := map[string]bool{}
	for _, d := range disable {
		off[strings.ToLower(strings.TrimSpace(d))] = true
	}
	var out []Skill
	for _, s := range all {
		if !off[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// Stage writes a read-only copy of list into dst (which must not exist or
// be empty): dst/<name>/<files>. Directories are 0555, files 0444.
func Stage(dst string, list []Skill) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("skills: stage: %w", err)
	}
	var dirs []string
	for _, s := range list {
		root := filepath.Join(dst, s.Name)
		paths := make([]string, 0, len(s.Files))
		for p := range s.Files {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			full := filepath.Join(root, filepath.FromSlash(p))
			if !strings.HasPrefix(full, root+string(filepath.Separator)) {
				return fmt.Errorf("skills: stage: bad path %q", p)
			}
			for d := filepath.Dir(full); d != dst && !slices.Contains(dirs, d); d = filepath.Dir(d) {
				dirs = append(dirs, d)
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return fmt.Errorf("skills: stage: %w", err)
			}
			if err := os.WriteFile(full, s.Files[p], 0o444); err != nil {
				return fmt.Errorf("skills: stage: %w", err)
			}
		}
	}
	// Make directories read-only last (deepest first), so writing succeeded.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		if err := os.Chmod(d, 0o555); err != nil {
			return fmt.Errorf("skills: stage: %w", err)
		}
	}
	return os.Chmod(dst, 0o555)
}

// Unstage removes a staged copy (restoring write permission first).
func Unstage(dst string) error {
	_ = filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o755)
		}
		return nil
	})
	return os.RemoveAll(dst)
}
