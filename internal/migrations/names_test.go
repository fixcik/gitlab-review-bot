package migrations

import (
	"reflect"
	"testing"
)

func TestNameVariants(t *testing.T) {
	for _, c := range []struct {
		name string
		want []string
	}{
		{"created_by", []string{"created_by", "createdBy", "CreatedBy"}},
		{"createdBy", []string{"createdBy", "created_by", "CreatedBy"}},
		{"orders", []string{"orders", "Orders", "Order"}},
		{"Order", []string{"Order", "order", "orders"}},
		{"order_items", []string{"order_items", "orderItems", "OrderItems", "OrderItem"}},
		{"Category", []string{"Category", "category", "categories"}},
		{"OrderItem", []string{"OrderItem", "order_item", "orderItem", "order_items"}},
		{"x", []string{"x", "X"}},
		{"bad name!", nil},
	} {
		if got := NameVariants(c.name); !reflect.DeepEqual(got, c.want) {
			t.Errorf("NameVariants(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMentionsAndUsesName(t *testing.T) {
	mig := "ALTER TABLE orders DROP COLUMN created_by;"
	for _, c := range []struct {
		text, name string
		want       bool
	}{
		{mig, "createdBy", true},
		{mig, "created_by", true},
		{mig, "Order", true},    // entity of the table
		{mig, "created", false}, // whole words only
		{mig, "customer_id", false},
		{"$o->createdByUser", "createdBy", false},
	} {
		if got := MentionsName(c.text, c.name); got != c.want {
			t.Errorf("MentionsName(%q, %q) = %v", c.text, c.name, got)
		}
	}
	if !UsesName("    private ?int $createdBy = null;", "created_by") {
		t.Error("code line")
	}
	if UsesName("    // created_by is legacy", "created_by") || UsesName(" * @var createdBy", "createdBy") {
		t.Error("comment lines must not count")
	}
	if UsesName("anything", "") {
		t.Error("empty name")
	}
}
