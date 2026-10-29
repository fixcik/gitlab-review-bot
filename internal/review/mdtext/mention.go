package mdtext

import (
	"regexp"
)

// UsernameRe: what a mention may contain (config validates the same).
var UsernameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
