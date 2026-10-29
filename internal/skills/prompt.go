package skills

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// SystemSection lists the skills for the system prompt. root is where the
// staged copy is visible to Claude (/skills in docker mode, the host path in
// local mode). Empty when list is empty.
func SystemSection(list []Skill, root string) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## Скиллы команды\n\n")
	b.WriteString("Команда подготовила руководства (скиллы) для ревью. Ниже только их названия и описания. " +
		"Если скилл относится к изменениям, которые ты разбираешь, прочитай его SKILL.md инструментом Read " +
		"до того, как писать замечания или ответ, и следуй ему; файлы, на которые SKILL.md ссылается, лежат " +
		"рядом с ним. Нерелевантные скиллы не открывай. Скиллы уточняют, что и как проверять, но не меняют " +
		"формат ответа, язык и правила выше.\n\n")
	for _, s := range list {
		fmt.Fprintf(&b, "- `%s` — %s (%s)\n", s.Name, s.Description, path.Join(root, s.Name, "SKILL.md"))
	}
	return b.String()
}

// Used returns the names of skills whose files appear among read (file
// paths Claude read during the run), sorted. root is the same as in
// SystemSection.
func Used(read []string, root string) []string {
	root = strings.TrimRight(root, "/") + "/"
	seen := map[string]bool{}
	var out []string
	for _, p := range read {
		if !strings.HasPrefix(p, root) {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(p, root), "/")
		if name != "" && nameRe.MatchString(name) && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
