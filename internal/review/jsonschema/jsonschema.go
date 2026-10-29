// Package jsonschema edits the JSON schemas of Claude's answers and parses
// the answers. Answer schemas are sent indented. The schemas are the bot's
// constants: one that does not parse is a bug, so the Must* helpers panic;
// EditProps returns the error for callers that fall back. It is a leaf: it
// must not depend on internal/review or any of its feature packages.
package jsonschema

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Unmarshal parses a JSON schema; name is for the error.
func Unmarshal(name, schema string) (map[string]any, error) {
	var s map[string]any
	if err := json.Unmarshal([]byte(schema), &s); err != nil {
		return nil, fmt.Errorf("review: bad %s: %w", name, err)
	}
	return s, nil
}

// Marshal marshals a schema indented.
func Marshal(s map[string]any) (string, error) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", fmt.Errorf("review: marshal schema: %w", err)
	}
	return string(b), nil
}

// Must parses a JSON schema constant; name is for the panic.
func Must(name, schema string) map[string]any {
	s, err := Unmarshal(name, schema)
	if err != nil {
		panic(err.Error())
	}
	return s
}

// MustMarshal is Marshal for a built-in schema.
func MustMarshal(s map[string]any) string {
	out, err := Marshal(s)
	if err != nil {
		panic(err.Error())
	}
	return out
}

// Edit parses a review schema, applies edit and marshals it back.
func Edit(schema string, edit func(s map[string]any)) string {
	s := Must("review schema", schema)
	edit(s)
	return MustMarshal(s)
}

// ErrNoProps: the schema has no top-level "properties" object.
var ErrNoProps = errors.New("review: review schema without properties")

// EditProps applies edit to the top-level "properties" of a JSON
// schema and re-marshals it. When edit reports no change the schema is
// returned as it was.
func EditProps(schema string, edit func(props map[string]any) bool) (string, error) {
	s, err := Unmarshal("review schema", schema)
	if err != nil {
		return "", err
	}
	props, ok := s["properties"].(map[string]any)
	if !ok {
		return "", ErrNoProps
	}
	if !edit(props) {
		return schema, nil
	}
	return Marshal(s)
}

// MustSetProp sets one top-level property of a built-in schema.
func MustSetProp(schema, name string, prop any) string {
	out, err := EditProps(schema, func(props map[string]any) bool {
		props[name] = prop
		return true
	})
	if err != nil {
		panic(err.Error())
	}
	return out
}

// UnmarshalStrict parses one JSON object, tolerating surrounding whitespace
// and a ```json fence.
func UnmarshalStrict(s string, v any) error {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	if s == "" {
		return errors.New("empty output")
	}
	if !strings.HasPrefix(s, "{") {
		return errors.New("output is not a JSON object")
	}
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	return nil
}
