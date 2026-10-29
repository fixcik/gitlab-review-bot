// Command rbadmin prints a user of rb-backend: rbadmin <user-id>.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

type user struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: rbadmin <user-id>")
		os.Exit(2)
	}
	base := os.Getenv("BACKEND_URL")
	if base == "" {
		base = "http://localhost:8080"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(base + "/api/v1/users/" + os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "status", resp.Status)
		os.Exit(1)
	}
	var u user
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s\t%s\t%s\n", u.ID, u.Email, u.Name)
}
