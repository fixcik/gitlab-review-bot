// Package greet builds greetings.
package greet

import "strings"

// Hello greets name; an empty name greets the world.
func Hello(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "world"
	}
	return "Hello, " + name + "!"
}

// Shout greets name in upper case.
func Shout(name string) string {
	return strings.ToUpper(Hello(name))
}
