package store

import (
	"context"
	"strings"
	"testing"
)

func TestBotLabels(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if a, sup, err := s.BotLabels(ctx, 1, 2); err != nil || a != nil || sup != nil {
		t.Fatalf("empty: %v %v %v", a, sup, err)
	}
	if err := s.SaveBotLabels(ctx, 1, 2, []string{"type::bug", "backend"}, []string{"db"}); err != nil {
		t.Fatal(err)
	}
	a, sup, err := s.BotLabels(ctx, 1, 2)
	if err != nil || strings.Join(a, ",") != "backend,type::bug" || strings.Join(sup, ",") != "db" {
		t.Fatalf("saved: %v %v %v", a, sup, err)
	}
	if a, _, _ := s.BotLabels(ctx, 1, 3); a != nil {
		t.Fatal("other mr")
	}
	// Replace: a label in both lists is suppressed; dropped labels vanish.
	if err := s.SaveBotLabels(ctx, 1, 2, []string{"backend", "db"}, []string{"db", "type::bug"}); err != nil {
		t.Fatal(err)
	}
	a, sup, _ = s.BotLabels(ctx, 1, 2)
	if strings.Join(a, ",") != "backend" || strings.Join(sup, ",") != "db,type::bug" {
		t.Fatalf("replaced: %v %v", a, sup)
	}
	if err := s.SaveBotLabels(ctx, 1, 2, nil, nil); err != nil {
		t.Fatal(err)
	}
	if a, sup, _ := s.BotLabels(ctx, 1, 2); a != nil || sup != nil {
		t.Fatalf("cleared: %v %v", a, sup)
	}
}

func TestLabelsMigrationRegistered(t *testing.T) {
	for _, m := range namedMigrations {
		if m.name == "mr_labels_v1" {
			return
		}
	}
	t.Fatal("mr_labels_v1 not registered")
}
