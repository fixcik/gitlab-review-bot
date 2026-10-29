package reactions

import "testing"

func TestClassify(t *testing.T) {
	const m = "@reviewbot"
	cases := []struct {
		text string
		want Kind
	}{
		// thanks, RU
		{"спасибо", Thanks},
		{"Спасибо!!!", Thanks},
		{"спасибо!", Thanks},
		{"СПАСИБО.", Thanks},
		{"спс", Thanks},
		{"спасибо большое", Thanks},
		{"Большое спасибо)))", Thanks},
		{"благодарю", Thanks},
		{"спасибо за ревью!", Thanks},
		{"@reviewbot спасибо", Thanks},
		{"спасибо @reviewbot", Thanks},
		{"@ReviewBot, спасибо!", Thanks},
		{"спасибо, бот", Thanks},
		{"**спасибо**", Thanks},
		// thanks, EN
		{"thanks", Thanks},
		{"Thanks!", Thanks},
		{"thx", Thanks},
		{"ty", Thanks},
		{"thank you very much", Thanks},
		{"Thanks a lot @reviewbot :)", Thanks},
		{"cheers", Thanks},
		// emoji
		{"🙏", Thanks},
		{"❤️", Thanks},
		{"спасибо 🙏🏻", Thanks},
		{":heart:", Thanks},
		{":pray: thanks", Thanks},
		// mixed: thanks wins over a plain ack
		{"ок, спасибо", Thanks},
		{"понял, спасибо!", Thanks},
		{"👍 спасибо", Thanks},
		{"ok thanks", Thanks},
		// ack
		{"ок", Ack},
		{"Ок, поправлю", Ack},
		{"согласен", Ack},
		{"понял", Ack},
		{"👍", Ack},
		{"👍🏽", Ack},
		{":thumbsup:", Ack},
		{":+1:", Ack},
		{"+1", Ack},
		{"lgtm", Ack},
		{"good catch, will fix", Ack},
		{"ok 🙂", Ack},
		{"да", Ack},
		// fix claims
		{"исправил", Fixed},
		{"Исправил!", Fixed},
		{"fixed", Fixed},
		{"done", Fixed},
		{"готово", Fixed},
		{"исправил, спасибо", Fixed},
		{"@reviewbot исправил", Fixed},
		{"уже поправил 👍", Fixed},
		// not pure
		{"", None},
		{"   ", None},
		{"@reviewbot", None},
		{"спасибо, а почему тут нужна блокировка?", None},
		{"спасибо, а почему тут нужна блокировка", None},
		{"thanks @reviewbot, but the cache is per request", None},
		{"спасибо @alice", None},
		{"спасибо?", None},
		{"ок?", None},
		{"не согласен", None},
		{"нет", None},
		{"исправил в другом MR", None},
		{"спасибо, но это не баг: ретраи делает шлюз", None},
		{"> спасибо", None},
		{"спасибо 😡", None},
		{":unknown_emoji: спасибо", None},
		{"`code`", None},
		{"ok, but please also check X", None},
		{"спасибо\n\nА ещё посмотри файл main.go", None},
		{"thanks for nothing", None},
	}
	for _, c := range cases {
		if got := Classify(c.text, m); got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestClassifyLongNote(t *testing.T) {
	long := ""
	for i := 0; i < 50; i++ {
		long += "спасибо "
	}
	if got := Classify(long, ""); got != None {
		t.Errorf("long note: %v", got)
	}
}

func TestClassifyWithoutMention(t *testing.T) {
	// Without a configured mention every @-word is content.
	if got := Classify("@reviewbot спасибо", ""); got != None {
		t.Errorf("got %v", got)
	}
	if got := Classify("спасибо", ""); got != Thanks {
		t.Errorf("got %v", got)
	}
}

// The acknowledgements learning used to skip (review/learning/learn.go trivialReply)
// all classify as something other than None.
func TestClassifyCoversLearnTrivialList(t *testing.T) {
	for _, s := range []string{"ok", "okay", "ок", "окей", "ага", "да", "fixed", "done", "thanks", "thank you", "thx", "lgtm",
		"спасибо", "спс", "исправил", "исправлено", "поправил", "готово", "сделал", "сделано", "принято", "согласен", "+1", "👍"} {
		if Classify(s, "") == None {
			t.Errorf("%q: None", s)
		}
	}
}
