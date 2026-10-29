package migrations

import (
	"regexp"
	"strings"
	"unicode"
)

// identRe: names the usage check can search for.
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// genericNames are column names too common to search for on their own: a
// match counts only in files that also mention the table.
var genericNames = map[string]bool{}

func init() {
	for _, n := range strings.Fields(`id uuid name type kind status state value data title code key label slug url
		description text body content comment note notes email phone date time datetime amount price total count number
		sort position order rank priority level active enabled disabled visible deleted hidden created updated
		created_at updated_at deleted_at user_id owner_id parent_id account_id
		company_id tenant_id version hash token password path file image size weight height width color source target
		result message error flags options settings config meta metadata extra info payload params attributes tags
		category group role permission start end from to min max lat lng lon city country address zip region`) {
		genericNames[n] = true
	}
}

// generic reports a name too common to search for alone.
func generic(name string) bool {
	return len(name) < 3 || genericNames[strings.ToLower(snake(splitWords(name)))]
}

// splitWords splits snake_case, kebab-case, camelCase and PascalCase into
// lower-case words.
func splitWords(s string) []string {
	var words []string
	var cur []rune
	rs := []rune(s)
	push := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = nil
		}
	}
	for i, r := range rs {
		switch {
		case r == '_' || r == '-' || r == ' ':
			push()
		case unicode.IsUpper(r):
			// "createdBy" -> created|By; "HTTPServer" -> HTTP|Server.
			if len(cur) > 0 && (!unicode.IsUpper(rs[i-1]) || i+1 < len(rs) && unicode.IsLower(rs[i+1])) {
				push()
			}
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	push()
	return words
}

func snake(ws []string) string { return strings.Join(ws, "_") }

func pascal(ws []string) string {
	var b strings.Builder
	for _, w := range ws {
		if w == "" {
			continue
		}
		r := []rune(w)
		b.WriteString(strings.ToUpper(string(r[0])) + string(r[1:]))
	}
	return b.String()
}

func camel(ws []string) string {
	p := pascal(ws)
	if p == "" {
		return ""
	}
	r := []rune(p)
	return strings.ToLower(string(r[0])) + string(r[1:])
}

// singular makes the last word singular (simple English rules).
func singular(ws []string) []string {
	if len(ws) == 0 {
		return ws
	}
	out := append([]string(nil), ws...)
	w := out[len(out)-1]
	switch {
	case strings.HasSuffix(w, "ies") && len(w) > 4:
		w = w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "sses"), strings.HasSuffix(w, "xes"), strings.HasSuffix(w, "ches"), strings.HasSuffix(w, "shes"):
		w = w[:len(w)-2]
	case strings.HasSuffix(w, "ss"), strings.HasSuffix(w, "us"), strings.HasSuffix(w, "is"):
	case strings.HasSuffix(w, "s") && len(w) > 3:
		w = w[:len(w)-1]
	}
	out[len(out)-1] = w
	return out
}

func addUnique(list []string, vs ...string) []string {
	for _, v := range vs {
		if v == "" || !identRe.MatchString(v) {
			continue
		}
		dup := false
		for _, x := range list {
			if x == v {
				dup = true
				break
			}
		}
		if !dup {
			list = append(list, v)
		}
	}
	return list
}

// columnVariants are the spellings of a column (or field) in code:
// created_by, createdBy, CreatedBy.
func columnVariants(name string) []string {
	ws := splitWords(name)
	return addUnique(nil, name, snake(ws), camel(ws), pascal(ws))
}

// tableVariants are the spellings of a table in code: the name, its
// camelCase, and the entity / model class (singular PascalCase):
// order_items, orderItems, OrderItems, OrderItem.
func tableVariants(name string) []string {
	ws := splitWords(name)
	out := addUnique(nil, name)
	if len(ws) > 1 {
		out = addUnique(out, snake(ws), camel(ws), pascal(ws))
	}
	if e := pascal(singular(ws)); len(e) >= 3 {
		out = addUnique(out, e)
	}
	return out
}

// plural makes the last word plural (simple English rules): order ->
// orders, category -> categories, address -> addresses.
func plural(ws []string) []string {
	if len(ws) == 0 {
		return ws
	}
	out := append([]string(nil), ws...)
	w := out[len(out)-1]
	switch {
	case w == "":
	case strings.HasSuffix(w, "y") && len(w) > 2 && !strings.ContainsAny(w[len(w)-2:len(w)-1], "aeiou"):
		w = w[:len(w)-1] + "ies"
	case strings.HasSuffix(w, "s"), strings.HasSuffix(w, "x"), strings.HasSuffix(w, "ch"), strings.HasSuffix(w, "sh"):
		w += "es"
	default:
		w += "s"
	}
	out[len(out)-1] = w
	return out
}

// NameVariants are the spellings under which a dropped or renamed table,
// column, field or model may appear in code and in migrations: the name,
// its snake_case / camelCase / PascalCase forms, the entity class
// (singular PascalCase) and, for a class name, the plural snake_case table
// name (createdBy -> created_by, CreatedBy; Order -> orders; order_items ->
// orderItems, OrderItem). Only identifiers are returned.
func NameVariants(name string) []string {
	out := addUnique(columnVariants(name), tableVariants(name)...)
	if ws := splitWords(name); len(ws) > 0 && unicode.IsUpper([]rune(name)[0]) {
		// An entity or model class: its conventional table name.
		out = addUnique(out, snake(plural(ws)))
	}
	return out
}

// MentionsName reports whether text contains name or one of its
// NameVariants as a whole word (case sensitive).
func MentionsName(text, name string) bool {
	re := wordsRe(NameVariants(name))
	return re != nil && re.MatchString(text)
}

// UsesName reports whether one line of code mentions name (see
// MentionsName) and is not only a comment.
func UsesName(line, name string) bool {
	return !commentLine(line) && MentionsName(line, name)
}
