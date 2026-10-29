// Command rbadmin prints a user of rb-backend: rbadmin <user-id>.
package main

import (
	"encoding/json"
	"flag"
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
	asJSON := flag.Bool("json", false, "print JSON")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: rbadmin [-json] <user-id>")
		os.Exit(2)
	}
	base := os.Getenv("BACKEND_URL")
	if base == "" {
		base = "http://localhost:8080"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(base + "/api/v1/users/" + flag.Arg(0))
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
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(u)
		return
	}
	fmt.Printf("%s\t%s\t%s\n", u.ID, u.Email, u.Name)
}
