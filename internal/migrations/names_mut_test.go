package migrations

import (
	"reflect"
	"testing"
)

func TestSplitWords(t *testing.T) {
	for in, want := range map[string][]string{
		"created_by":    {"created", "by"},
		"createdBy":     {"created", "by"},
		"CreatedBy":     {"created", "by"},
		"HTTPServer":    {"http", "server"},
		"userID":        {"user", "id"},
		"order-items":   {"order", "items"},
		"__a__b":        {"a", "b"},
		"X":             {"x"},
		"two words":     {"two", "words"},
		"getHTTPCode2x": {"get", "http", "code2x"},
		"xCoord":        {"x", "coord"},
	} {
		if got := splitWords(in); !reflect.DeepEqual(got, want) {
			t.Errorf("splitWords(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSingularPlural(t *testing.T) {
	for in, want := range map[string]string{
		"categories": "category", "ties": "tie", "addresses": "address", "boxes": "box", "matches": "match",
		"dishes": "dish", "status": "status", "analysis": "analysis", "class": "class", "orders": "order",
		"gas": "gas", "bus": "bus", "users": "user",
	} {
		if got := singular([]string{"x", in}); got[1] != want || got[0] != "x" {
			t.Errorf("singular(%q) = %v, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"category": "categories", "day": "days", "key": "keys", "box": "boxes", "match": "matches",
		"dish": "dishes", "address": "addresses", "order": "orders", "y": "ys", "toy": "toys",
	} {
		if got := plural([]string{"x", in}); got[1] != want || got[0] != "x" {
			t.Errorf("plural(%q) = %v, want %q", in, got, want)
		}
	}
	if singular(nil) != nil || plural(nil) != nil {
		t.Error("empty word lists")
	}
	// The input is not modified.
	ws := []string{"orders"}
	singular(ws)
	plural(ws)
	if ws[0] != "orders" {
		t.Errorf("input modified: %v", ws)
	}
}

func TestNameVariantsMore(t *testing.T) {
	for _, c := range []struct {
		name string
		want []string
	}{
		{"Address", []string{"Address", "address", "addresses"}},
		{"addresses", []string{"addresses", "Addresses", "Address"}},
		{"user_categories", []string{"user_categories", "userCategories", "UserCategories", "UserCategory"}},
		{"HTTPLog", []string{"HTTPLog", "http_log", "httpLog", "HttpLog", "http_logs"}},
		{"ab", []string{"ab", "Ab"}}, // entity names shorter than 3 are not added
		{"", nil},
	} {
		if got := NameVariants(c.name); !reflect.DeepEqual(got, c.want) {
			t.Errorf("NameVariants(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestGenericNames(t *testing.T) {
	for n, want := range map[string]bool{
		"CreatedAt": true, "userId": true, "USER_ID": true, "xy": true, "Status": true,
		"abc": false, "order_status": false, "sessions": false,
	} {
		if generic(n) != want {
			t.Errorf("generic(%q) = %v", n, !want)
		}
	}
}

// TestMentionsNameEvidence pins the evidence matching used before a
// blocking migration finding stands: whole words only, case sensitive,
// through the name's spellings.
func TestMentionsNameEvidence(t *testing.T) {
	mig := "Schema::table('order_items', function ($t) { $t->dropColumn('unit_price'); });"
	for _, c := range []struct {
		name string
		want bool
	}{
		{"unitPrice", true},
		{"UnitPrice", true},
		{"OrderItem", true},  // entity of the table
		{"orderItems", true}, // camelCase of the table
		{"unit", false},
		{"price", false},
		{"Order", false},     // "orders" is not in the text
		{"UNIT_PRICE", true}, // its snake_case spelling is unit_price
		{"unit_price_v2", false},
	} {
		if got := MentionsName(mig, c.name); got != c.want {
			t.Errorf("MentionsName(%q) = %v", c.name, got)
		}
	}
	if !UsesName("total = item.unit_price * qty  # price", "unitPrice") {
		t.Error("code line with a trailing comment uses the name")
	}
	if UsesName("# item.unit_price", "unit_price") || UsesName("-- unit_price", "unit_price") {
		t.Error("comment-only lines do not use the name")
	}
	if UsesName("$x->unitPriceOld", "unit_price") {
		t.Error("longer identifier is not the name")
	}
}
