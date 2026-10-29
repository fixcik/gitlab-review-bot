package mcpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// DecodeArgs unmarshals tool arguments into v, refusing unknown fields and
// trailing data, with an error text meant for the model.
func DecodeArgs(args json.RawMessage, v any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		msg := err.Error()
		msg = strings.TrimPrefix(msg, "json: ")
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			msg = fmt.Sprintf("argument %q must be %s", te.Field, jsonKind(te.Type.Kind().String()))
		}
		return fmt.Errorf("invalid arguments: %s", msg)
	}
	if dec.More() {
		return errors.New("invalid arguments: trailing data")
	}
	return nil
}

func jsonKind(goKind string) string {
	switch goKind {
	case "int", "int64", "int32":
		return "an integer"
	case "string":
		return "a string"
	case "bool":
		return "a boolean"
	case "slice":
		return "an array"
	case "struct", "ptr", "map":
		return "an object"
	}
	return "of another type"
}
