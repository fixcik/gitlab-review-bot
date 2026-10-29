// Package reactions puts award emoji (GitLab "reactions") on the notes
// people address to the bot and on MRs: 👀 when a command or question is
// picked up, ✅ / ❌ / ⏳ / 🚫 for how it ended, ❤️ for a plain "thanks",
// 👍 for an acknowledgement in a bot thread, 🧠 when a rule was learned and
// 🎉 for a clean review. See docs/features/reactions.md.
//
// The package has two parts: Classify, a deterministic, conservative
// classifier of "pure thanks / acknowledgement" notes (shared by the webhook
// router and the reviewer), and Reactor, an ordered background worker that
// applies reactions idempotently and never fails a job.
package reactions

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Kind is what a short note says, from the bot's point of view.
type Kind int

const (
	// None: anything else (a question, a request, an explanation, other
	// mentions, unknown emoji, ...). The note is handled as before.
	None Kind = iota
	// Thanks: only thanks ("спасибо!", "thx", "🙏"), possibly with the
	// bot's mention and acknowledgements ("ок, спасибо").
	Thanks
	// Ack: only an acknowledgement ("ок", "согласен", "поправлю", "👍").
	Ack
	// Fixed: an acknowledgement that claims a fix ("исправил", "fixed",
	// "done"). Addressed to the bot it stays a chat question (the chat run
	// re-checks the code and may resolve the thread); as a plain reply in a
	// bot thread it is treated like Ack.
	Fixed
)

func (k Kind) String() string {
	switch k {
	case Thanks:
		return "thanks"
	case Ack:
		return "ack"
	case Fixed:
		return "fixed"
	}
	return "none"
}

// maxClassifyRunes: longer notes are never "pure" thanks or acks.
const maxClassifyRunes = 200

// Phrases, lower-case, "ё" written as "е", words separated by one space.
var (
	thanksPhrases = phraseSet(
		"спасибо", "спс", "спасиб", "спасибки", "спасибочки", "пасиб", "пасибо", "благодарю", "благодарствую",
		"мерси", "сенкс", "сенкью", "сенк ю", "спасибо большое", "большое спасибо", "огромное спасибо",
		"спасибо огромное", "спасибище", "спасибо за ревью", "спасибо за ответ", "спасибо за помощь",
		"спасибо за подсказку", "спасибо за замечание", "спасибо за пояснение", "спасибо за разъяснение",
		"благодарю за ревью", "благодарю за ответ", "благодарю за помощь",
		"thanks", "thank you", "thank u", "thanx", "thnx", "thx", "tnx", "ty", "tysm", "thanks a lot",
		"thanks so much", "thank you so much", "thank you very much", "thanks very much", "many thanks",
		"much appreciated", "appreciated", "cheers", "thanks for the review", "thank you for the review",
		"thanks for the help", "thank you for the help", "thanks for the explanation",
		"thank you for the explanation", "thanks for the answer", "thank you for the answer",
	)
	ackPhrases = phraseSet(
		"ок", "окей", "оке", "окс", "ok", "okay", "okey", "кк", "ага", "угу", "да", "yes", "yep", "yup",
		"понял", "поняла", "поняли", "понятно", "ясно", "принято", "принял", "приняла", "согласен",
		"согласна", "согласны", "справедливо", "резонно", "верно", "точно", "хорошо", "хорошее замечание",
		"поправлю", "исправлю", "поправим", "исправим", "сейчас поправлю", "сейчас исправлю", "учту", "учтем",
		"учтено", "сделаю", "сделаем", "поправлю позже", "agreed", "agree", "got it", "makes sense", "fair",
		"fair point", "good catch", "nice catch", "good point", "will fix", "will do", "noted", "ack",
		"sure", "lgtm", "+1", "sounds good",
	)
	fixedPhrases = phraseSet(
		"исправил", "исправила", "исправили", "исправлено", "поправил", "поправила", "поправили",
		"поправлено", "сделал", "сделала", "сделали", "сделано", "готово", "уже исправил", "уже поправил",
		"уже исправлено", "fixed", "fixed it", "done", "resolved", "addressed",
	)
	// neutralPhrases may appear around thanks or an ack without changing it.
	neutralPhrases = phraseSet("бот", "bot", "ботик")
)

// maxPhraseWords is the longest phrase above.
const maxPhraseWords = 5

// Emoji (base code points; variation selectors, ZWJ and skin tones are
// ignored). An emoji outside these sets makes the note None.
var (
	thanksEmoji  = runeSet("❤♥💙💚💛💜🧡🖤🤍🤎💗💖💕💞💓💝🙏🤗😊☺🥰🫶😍")
	ackEmoji     = runeSet("👍👌✅✔🆗🤝💯")
	neutralEmoji = runeSet("🙂😀😃😄😁😉")
)

// shortcodes are GitLab's text emoji (":thumbsup:") mapped to the emoji
// they render as.
var shortcodes = map[string]rune{
	"heart": '❤', "hearts": '♥', "pray": '🙏', "hugging": '🤗', "hugs": '🤗', "blush": '😊',
	"relaxed": '☺', "smiling_face_with_hearts": '🥰', "heart_eyes": '😍', "heart_hands": '🫶',
	"thumbsup": '👍', "+1": '👍', "thumbs_up": '👍', "ok_hand": '👌', "white_check_mark": '✅',
	"heavy_check_mark": '✔', "ok": '🆗', "handshake": '🤝', "100": '💯',
	"slight_smile": '🙂', "smile": '😄', "smiley": '😃', "grinning": '😀', "grin": '😁', "wink": '😉',
}

var shortcodeRe = regexp.MustCompile(`:([a-z0-9_+\-]{1,40}):`)

// Classify tells whether text (a note, or the text after the bot's
// mention) only thanks or acknowledges. It is deliberately conservative:
// a question mark, another mention, a word or emoji it does not know, or
// more than maxClassifyRunes runes give None. mention is the bot's mention
// ("@reviewbot", case-insensitive); it is ignored wherever it appears.
// Precedence: a fix claim wins over thanks ("исправил, спасибо" is Fixed),
// thanks over a plain acknowledgement ("ок, спасибо" is Thanks).
func Classify(text, mention string) Kind {
	if utf8.RuneCountInString(text) > maxClassifyRunes {
		return None
	}
	s := strings.ToLower(text)
	s = strings.ReplaceAll(s, "ё", "е")
	if strings.ContainsAny(s, "?¿") {
		return None
	}
	unknown := false
	s = shortcodeRe.ReplaceAllStringFunc(s, func(m string) string {
		if r, ok := shortcodes[strings.Trim(m, ":")]; ok {
			return " " + string(r) + " "
		}
		unknown = true
		return m
	})
	if unknown {
		return None
	}
	mention = strings.ToLower(strings.TrimSpace(mention))

	var thanks, ack, fixed bool
	var words []string
	flush := func() bool {
		if len(words) == 0 {
			return true
		}
		ok := matchWords(words, &thanks, &ack, &fixed)
		words = words[:0]
		return ok
	}
	var word strings.Builder
	endWord := func() bool {
		w := strings.Trim(word.String(), "-")
		word.Reset()
		if w == "" {
			return true
		}
		if strings.HasPrefix(w, "@") {
			// The bot's own mention is not content; any other one is.
			return mention != "" && w == mention
		}
		words = append(words, w)
		return true
	}
	for _, r := range s {
		switch {
		case isEmojiModifier(r):
			// part of the previous emoji
		case unicode.IsSpace(r):
			if !endWord() {
				return None
			}
		case isSeparator(r):
			if !endWord() || !flush() {
				return None
			}
		case isEmoji(r):
			if !endWord() || !flush() {
				return None
			}
			switch {
			case thanksEmoji[r]:
				thanks = true
			case ackEmoji[r]:
				ack = true
			case neutralEmoji[r]:
			default:
				return None
			}
		default:
			word.WriteRune(r)
		}
	}
	if !endWord() || !flush() {
		return None
	}
	switch {
	case fixed:
		return Fixed
	case thanks:
		return Thanks
	case ack:
		return Ack
	}
	return None
}

// matchWords covers words with known phrases, longest first. It fails when
// some word is not part of a phrase.
func matchWords(words []string, thanks, ack, fixed *bool) bool {
	for i := 0; i < len(words); {
		n := 0
	longest:
		for l := min(maxPhraseWords, len(words)-i); l > 0; l-- {
			p := strings.Join(words[i:i+l], " ")
			switch {
			case fixedPhrases[p]:
				*fixed = true
			case thanksPhrases[p]:
				*thanks = true
			case ackPhrases[p]:
				*ack = true
			case neutralPhrases[p]:
			default:
				continue
			}
			n = l
			break longest
		}
		if n == 0 {
			return false
		}
		i += n
	}
	return true
}

// isSeparator: punctuation between phrases ("ок, поправлю!", "спасибо)))",
// markdown emphasis). Not '?' (handled before), not '@', '+', '-' or '>'
// (quotes are content).
func isSeparator(r rune) bool {
	switch r {
	case ',', '.', '!', ';', ':', '…', '(', ')', '[', ']', '"', '\'', '«', '»', '—', '–', '*', '_', '~', '`', '=', '/', '\\':
		return true
	}
	return false
}

func isEmojiModifier(r rune) bool {
	return r == 0xFE0F || r == 0xFE0E || r == 0x200D || r == 0x20E3 || (r >= 0x1F3FB && r <= 0x1F3FF)
}

func isEmoji(r rune) bool {
	return r >= 0x1F000 || (r >= 0x2190 && r <= 0x2BFF) || unicode.Is(unicode.So, r)
}

func phraseSet(ps ...string) map[string]bool {
	m := make(map[string]bool, len(ps))
	for _, p := range ps {
		m[p] = true
	}
	return m
}

func runeSet(s string) map[rune]bool {
	m := map[rune]bool{}
	for _, r := range s {
		if !isEmojiModifier(r) {
			m[r] = true
		}
	}
	return m
}
