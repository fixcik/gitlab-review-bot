package tracing

import (
	"fmt"
	"time"
)

// Kind is the OTLP span kind (the enum values are the wire values).
type Kind int

// Span kinds.
const (
	KindUnspecified Kind = 0
	KindInternal    Kind = 1
	KindServer      Kind = 2
	KindClient      Kind = 3
	KindProducer    Kind = 4
	KindConsumer    Kind = 5
)

// StatusCode is the OTLP status code (wire values).
type StatusCode int

// Status codes.
const (
	StatusUnset StatusCode = 0
	StatusOK    StatusCode = 1
	StatusError StatusCode = 2
)

// ValueType tells which field of Value is set.
type ValueType int

// Value types.
const (
	TypeString ValueType = iota
	TypeInt
	TypeBool
	TypeFloat
)

// Value is an attribute value: string, int64, bool or float64.
type Value struct {
	Type ValueType
	Str  string
	Int  int64
	Bool bool
	F    float64
}

// Attr is a key/value attribute.
type Attr struct {
	Key   string
	Value Value
}

// String makes a string attribute.
func String(k, v string) Attr { return Attr{k, Value{Type: TypeString, Str: v}} }

// Int makes an integer attribute.
func Int(k string, v int) Attr { return Attr{k, Value{Type: TypeInt, Int: int64(v)}} }

// Int64 makes an integer attribute.
func Int64(k string, v int64) Attr { return Attr{k, Value{Type: TypeInt, Int: v}} }

// Bool makes a boolean attribute.
func Bool(k string, v bool) Attr { return Attr{k, Value{Type: TypeBool, Bool: v}} }

// Float64 makes a double attribute.
func Float64(k string, v float64) Attr { return Attr{k, Value{Type: TypeFloat, F: v}} }

// AsString returns the value as text (for logs and tests).
func (v Value) AsString() string {
	switch v.Type {
	case TypeInt:
		return fmt.Sprint(v.Int)
	case TypeBool:
		return fmt.Sprint(v.Bool)
	case TypeFloat:
		return fmt.Sprint(v.F)
	}
	return v.Str
}

// Event is a timestamped annotation of a span.
type Event struct {
	Name  string
	Time  time.Time
	Attrs []Attr
}

// Link points at another span (here: the event that produced a job).
type Link struct {
	SpanContext SpanContext
	Attrs       []Attr
}

func findAttr(attrs []Attr, key string) (Value, bool) {
	for _, a := range attrs {
		if a.Key == key {
			return a.Value, true
		}
	}
	return Value{}, false
}
