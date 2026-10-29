// Keeps the fixture sources (Go code of the test projects pushed to the local
// GitLab by e2e/local/seed.sh) out of the reviewbot module: `go test ./...` and
// `go vet ./...` of the repository do not descend into a nested module.
// The fixtures are checked by e2e/local/check-fixtures.sh instead.
module reviewbot.local/fixtures

go 1.24
