package users

import (
	"fmt"
	"os"
)

// AppendAudit appends a line to the audit file.
func AppendAudit(path, line string) {
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	fmt.Fprintln(f, line)
	f.Close()
}
