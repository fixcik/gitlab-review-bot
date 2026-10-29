package memory

import "testing"

// remove: "replaces" names the bullet to drop when given (text may then be
// a reason), and a short fragment (< 8 key characters) never matches by
// substring, so a vague reference cannot delete an unrelated bullet.
func TestMutRemoveTargets(t *testing.T) {
	mem := map[string][]string{SectionTest: {
		"Интеграционные тесты: `make itest` нужен docker",
		"Юнит-тесты: `go test ./internal/...`",
	}}
	ch := Apply(mem, []Update{{Section: "test", Op: "remove",
		Replaces: "Интеграционные тесты: `make itest` нужен docker",
		Text:     "Юнит-тесты: `go test ./internal/...`"}})
	if len(ch) != 1 || len(mem[SectionTest]) != 1 || mem[SectionTest][0] != "Юнит-тесты: `go test ./internal/...`" {
		t.Fatalf("remove by replaces: %+v %q", ch, mem[SectionTest])
	}
	if ch := Apply(mem, []Update{{Section: "test", Op: "remove", Text: "go"}}); len(ch) != 0 || len(mem[SectionTest]) != 1 {
		t.Fatalf("short fragment removed something: %+v %q", ch, mem[SectionTest])
	}
	if ch := Apply(mem, []Update{{Section: "test", Op: "remove", Text: "go test ./internal"}}); len(ch) != 1 || len(mem[SectionTest]) != 0 {
		t.Fatalf("long fragment did not match: %+v %q", ch, mem[SectionTest])
	}
}
