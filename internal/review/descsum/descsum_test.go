package descsum

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

func sample() *output.DescriptionSummary {
	return &output.DescriptionSummary{
		Overview: "Добавляет кэш заказов и\nисправляет пагинацию.",
		Groups: []output.SummaryGroup{
			{Kind: "fix", Items: []string{"пагинация больше не теряет последнюю страницу"}},
			{Kind: "new", Items: []string{"LRU-кэш заказов на 5 минут", "  "}},
			{Kind: "docs", Items: nil},
			{Kind: "weird", Items: []string{"/close это не команда", "пингуем @alice и #12, ссылка https://evil.example/x [клик](http://evil)",
				"правка в /work/cache.go и /repos/g/web/api.ts"}},
		},
	}
}

func TestRender(t *testing.T) {
	got := Render(sample(), "Russian")
	for _, s := range []string{Start + "\n", "\n" + End, "**Саммари от ревью-бота**", "Добавляет кэш заказов и исправляет пагинацию.",
		"**Новое**\n- LRU-кэш заказов на 5 минут", "**Исправления**\n- пагинация", "**Прочее**\n"} {
		if !strings.Contains(got, s) {
			t.Errorf("block lacks %q:\n%s", s, got)
		}
	}
	// Groups in a fixed order, empty ones dropped; no live mentions,
	// references, links or quick actions from the model.
	if strings.Index(got, "**Новое**") > strings.Index(got, "**Исправления**") || strings.Contains(got, "Документация") {
		t.Errorf("order or empty groups:\n%s", got)
	}
	for _, bad := range []string{"@alice", "#12", "https://", "http://", "[клик]", "\n/close", "- /close", "/work/", "/repos/"} {
		if strings.Contains(got, bad) {
			t.Errorf("live %q in:\n%s", bad, got)
		}
	}
	if Render(nil, "Russian") != "" || Render(&output.DescriptionSummary{Overview: " "}, "Russian") != "" {
		t.Error("an empty summary renders nothing")
	}
	if en := Render(sample(), "English"); !strings.Contains(en, "**Summary by the review bot**") || !strings.Contains(en, "**New**") || !strings.Contains(en, "**Fixes**") {
		t.Errorf("english:\n%s", en)
	}
	// Caps: items per group, item length, overview length, the block size.
	big := &output.DescriptionSummary{Overview: strings.Repeat("о", 2000)}
	for i := 0; i < 20; i++ {
		big.Groups = append(big.Groups, output.SummaryGroup{Kind: "new", Items: []string{strings.Repeat("п", 1000)}})
	}
	if b := Render(big, "Russian"); len(b) > MaxBlockBytes || strings.Count(b, "\n- ") != maxItems {
		t.Errorf("caps: %d bytes, %d items", len(b), strings.Count(b, "\n- "))
	}
	// Every group full of long items: trimmed from the end to the size cap.
	huge := &output.DescriptionSummary{Overview: "x"}
	for _, k := range []string{"new", "fix", "refactor", "test", "docs", "other"} {
		g := output.SummaryGroup{Kind: k}
		for i := 0; i < maxItems; i++ {
			g.Items = append(g.Items, strings.Repeat("ж", maxItemRunes))
		}
		huge.Groups = append(huge.Groups, g)
	}
	if b := Render(huge, "Russian"); len(b) > MaxBlockBytes || !strings.HasSuffix(b, End) || !strings.Contains(b, "**Новое**") {
		t.Errorf("trim: %d bytes", len(b))
	}
}

func TestMerge(t *testing.T) {
	block := Render(sample(), "Russian")
	other := strings.Replace(block, "LRU-кэш", "TTL-кэш", 1)
	for _, c := range []struct {
		name, desc string
		written    bool
		want       string
		why        string
	}{
		{"empty", "", false, block, ""},
		{"author text", "Описание автора.\n\nЕщё абзац.\n\n", false, "Описание автора.\n\nЕщё абзац.\n\n" + block, ""},
		{"replace in place", "До\n\n" + other + "\n\nПосле", true, "До\n\n" + block + "\n\nПосле", ""},
		{"same", "Текст\n\n" + block, true, "", WhySame},
		{"off", "Текст\n<!-- reviewbot:description-summary-off -->", false, "", WhyOff},
		{"off with a block", "Текст\n<!-- reviewbot:description-summary-off -->\n\n" + other, true, "", WhyOff},
		{"removed by the author", "Текст автора", true, "", WhyRemoved},
		{"broken markers", "Текст " + Start + " без конца", false, "", WhyBroken},
		{"quick action of the author", "Текст\n/assign @bob\n", false, "", WhyQuickActions},
		{"slash text is no quick action", "Путь\n/api/v1/orders меняется", false, "Путь\n/api/v1/orders меняется\n\n" + block, ""},
	} {
		got, why := Merge(c.desc, block, c.written)
		if got != c.want || why != c.why {
			t.Errorf("%s: %q %q\nwant %q %q", c.name, got, why, c.want, c.why)
		}
	}
}

func TestNormalize(t *testing.T) {
	out := &output.ReviewOutput{DescriptionSummary: sample()}
	Normalize(out, false)
	if out.DescriptionSummary != nil {
		t.Error("dropped when off")
	}
	out = &output.ReviewOutput{DescriptionSummary: sample()}
	Normalize(out, true)
	if out.DescriptionSummary == nil {
		t.Error("kept when on")
	}
}

func TestSchemaAndRules(t *testing.T) {
	base := `{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`
	if WithSchema(base, false) != base {
		t.Error("off: schema unchanged")
	}
	if s := WithSchema(base, true); !strings.Contains(s, `"description_summary"`) || !strings.Contains(s, `"refactor"`) {
		t.Errorf("schema: %s", s)
	}
	if Rules(false, "Russian") != "" || !strings.Contains(Rules(true, "Russian"), "in Russian") {
		t.Error("rules")
	}
}
