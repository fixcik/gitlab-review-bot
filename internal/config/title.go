package config

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The MR title suggestion (jira.title_prompt, jira.title_language; the
// repository's jira section overrides the service). The prompt is plain
// text with two placeholders, {types} and {language}, replaced literally
// (never a template). Like the repository's instructions it is written by
// the people who run the bot or the project's maintainers and goes into the
// review prompt as their instructions; it is bounded and validated.

// MaxTitlePromptBytes caps jira.title_prompt.
const MaxTitlePromptBytes = 4000

// DefaultTitleLanguage is the language of suggested titles by default,
// whatever review.default_language is.
const DefaultTitleLanguage = "en"

// DefaultTitlePrompt is the built-in title format (jira.title_prompt empty).
const DefaultTitlePrompt = "Use Conventional Commits: `type(scope): description`, where type is one of: {types}. " +
	"The scope is optional; when used, name the module or subsystem the change belongs to, not a file name. " +
	"Write the description in {language}, in the imperative mood and present tense (\"add cache\", not \"added cache\"), " +
	"starting with a lowercase letter, without a trailing period. Keep the whole title within 72 characters. " +
	"Describe the essence of the change, not a list of the changed files. " +
	"Do not include the Jira issue key (the bot adds it) and do not use emoji."

var (
	titlePlaceholderRe = regexp.MustCompile(`\{([A-Za-z_]+)\}`)
	titleLanguageRe    = regexp.MustCompile(`^[A-Za-z][A-Za-z -]{0,39}$`)
)

// languageNames turns common codes into the language names a model reads
// best; other values are used as written.
var languageNames = map[string]string{"en": "English", "ru": "Russian", "de": "German", "fr": "French", "es": "Spanish", "uk": "Ukrainian"}

// TitleInstructions renders the title format: the prompt (or
// DefaultTitlePrompt when empty) with {types} and {language} replaced.
func TitleInstructions(prompt, language string, types []string) string {
	if strings.TrimSpace(prompt) == "" {
		prompt = DefaultTitlePrompt
	}
	return strings.NewReplacer("{types}", strings.Join(types, ", "), "{language}", TitleLanguageName(language)).Replace(strings.TrimSpace(prompt))
}

// TitleLanguageName is the language of suggested titles as the model reads
// it best ("en" -> "English"; unknown values as written; "" = English).
func TitleLanguageName(language string) string {
	lang := titleLanguage(language)
	if n, ok := languageNames[strings.ToLower(lang)]; ok {
		return n
	}
	return lang
}

// validateTitle checks a title prompt and language (key is "jira").
func validateTitle(key, prompt, language string) []string {
	var errs []string
	if len(prompt) > MaxTitlePromptBytes {
		errs = append(errs, fmt.Sprintf("%s.title_prompt: at most %d bytes", key, MaxTitlePromptBytes))
	}
	if !utf8.ValidString(prompt) || strings.IndexFunc(prompt, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r' }) >= 0 {
		errs = append(errs, key+".title_prompt: control characters are not allowed")
	}
	for _, m := range titlePlaceholderRe.FindAllStringSubmatch(prompt, -1) {
		if m[1] != "types" && m[1] != "language" {
			errs = append(errs, fmt.Sprintf("%s.title_prompt: unknown placeholder %q (use {types}, {language})", key, m[0]))
			break
		}
	}
	if l := strings.TrimSpace(language); l != "" && !titleLanguageRe.MatchString(l) {
		errs = append(errs, fmt.Sprintf("%s.title_language: %q is not a language name or code (letters, spaces, -)", key, language))
	}
	return errs
}

// titleLanguage is the language or DefaultTitleLanguage when empty.
func titleLanguage(l string) string {
	if l = strings.TrimSpace(l); l != "" {
		return l
	}
	return DefaultTitleLanguage
}
