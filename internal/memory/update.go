package memory

import (
	"strings"
)

// Operations of an update.
const (
	OpAdd     = "add"
	OpReplace = "replace"
	OpRemove  = "remove"
)

// Update is one entry of memory_updates in Claude's answer.
type Update struct {
	Section  string `json:"section"`
	Op       string `json:"op"`
	Text     string `json:"text"`
	Replaces string `json:"replaces,omitempty"`
	// Command is the exact command a build/test/lint/setup bullet is about
	// (when it worked). Used only to SUGGEST .reviewbot.yaml lines.
	Command string `json:"command,omitempty"`
}

// Change is an applied update.
type Change struct {
	Section string
	Op      string
	Text    string // the stored bullet (for remove: the removed one)
	Command string // Update.Command, trimmed (not validated here)
}

// Apply merges updates into mem (section key -> bullets, oldest first).
// Updates are cleaned (secrets scrubbed) and deduplicated; an unknown
// section goes to "other", an unknown op is ignored. Added and replaced
// bullets move to the end (newest). It returns the changes that actually
// modified something, in order.
func Apply(mem map[string][]string, updates []Update) []Change {
	var changes []Change
	for _, u := range updates {
		key := ParseSection(u.Section)
		if key == "" {
			key = SectionOther
		}
		var (
			ch Change
			ok bool
		)
		switch op := strings.ToLower(strings.TrimSpace(u.Op)); op {
		case OpAdd, OpReplace:
			ch, ok = applyAdd(mem, key, op, u)
		case OpRemove:
			ch, ok = applyRemove(mem, key, u)
		}
		if ok {
			changes = append(changes, ch)
		}
	}
	return changes
}

// applyAdd adds (or, for a match, replaces) the bullet of an add/replace
// update in section key. ok is false when nothing was reported: an empty
// text, or the same fact already stored (then only moved to the end).
func applyAdd(mem map[string][]string, key, op string, u Update) (Change, bool) {
	cur := mem[key]
	text := CleanBullet(u.Text)
	if text == "" {
		return Change{}, false
	}
	idx := -1
	if op == OpReplace && strings.TrimSpace(u.Replaces) != "" {
		idx = find(cur, u.Replaces)
	}
	if idx < 0 {
		idx = findSimilar(cur, text)
	}
	if idx >= 0 && Key(cur[idx]) == Key(text) {
		// Same fact (modulo case, punctuation, backticks): keep the stored
		// wording, move it to the end (newest), not reported.
		kept := cur[idx]
		mem[key] = append(append(cur[:idx:idx], cur[idx+1:]...), kept)
		return Change{}, false
	}
	if idx >= 0 {
		cur = append(cur[:idx:idx], cur[idx+1:]...)
		op = OpReplace
	} else {
		op = OpAdd
	}
	// Drop other near-duplicates of the new text.
	kept := cur[:0:0]
	for _, b := range cur {
		if !Similar(b, text) {
			kept = append(kept, b)
		}
	}
	mem[key] = append(kept, text)
	return Change{Section: key, Op: op, Text: text, Command: strings.Join(strings.Fields(u.Command), " ")}, true
}

// applyRemove removes the bullet a remove update refers to (Replaces, else
// Text). ok is false when no bullet matches.
func applyRemove(mem map[string][]string, key string, u Update) (Change, bool) {
	cur := mem[key]
	target := u.Replaces
	if strings.TrimSpace(target) == "" {
		target = u.Text
	}
	idx := find(cur, target)
	if idx < 0 {
		return Change{}, false
	}
	removed := cur[idx]
	mem[key] = append(cur[:idx:idx], cur[idx+1:]...)
	return Change{Section: key, Op: OpRemove, Text: removed}, true
}

// find locates the bullet a "replaces"/"remove" text refers to: same or
// similar text, or (for a fragment of at least 8 characters) a bullet whose
// key contains the fragment's key.
func find(bullets []string, ref string) int {
	ref = CleanBullet(ref)
	if ref == "" {
		return -1
	}
	if i := findSimilar(bullets, ref); i >= 0 {
		return i
	}
	k := Key(ref)
	if len([]rune(k)) < 8 {
		return -1
	}
	for i := len(bullets) - 1; i >= 0; i-- {
		if strings.Contains(Key(bullets[i]), k) {
			return i
		}
	}
	return -1
}

func findSimilar(bullets []string, text string) int {
	for i := len(bullets) - 1; i >= 0; i-- {
		if Similar(bullets[i], text) {
			return i
		}
	}
	return -1
}
