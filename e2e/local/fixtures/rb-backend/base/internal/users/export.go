package users

import (
	"encoding/csv"
	"io"
)

// WriteCSV writes users as CSV with a header row.
func WriteCSV(w io.Writer, list []User) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"id", "email", "name"}); err != nil {
		return err
	}
	for _, u := range list {
		if err := cw.Write([]string{u.ID, u.Email, u.Name}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// legacyCSVHeader was used by the old export job.
func legacyCSVHeader() string {
	return "id;email;name"
}
