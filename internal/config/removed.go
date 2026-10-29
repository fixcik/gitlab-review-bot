package config

import (
	"errors"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"
)

// Keys that no longer exist. Unknown keys are otherwise ignored, but these
// used to change what the review model could do (run commands, prepare the
// repository with network access, weaken the sandbox): silently ignoring
// them would leave people believing the bot still runs `go test` or
// `composer install`. A config that still has one of them is refused with
// a pointer to the reason.

// RemovedKeyReason is the explanation of the removed keys of the
// command-running review model (setup, tools, the Bash sandbox switches).
const RemovedKeyReason = "removed: the review model no longer runs commands; see docs/security.md"

// RemovedRepoCommandsReason is the explanation of compat.commands: the bot
// ran them in the MR checkout, i.e. it executed code the MR can change.
// Since the sandbox has no language runtimes and nothing from the MR may
// execute, repository-defined commands are gone like lint.custom.
const RemovedRepoCommandsReason = "removed: the bot no longer runs repository commands (code from the MR never executes); see docs/security.md"

// RemovedRepoModelReason is the explanation of the repository's model
// settings: which models review (and when the light one is used) is the
// operator's decision alone, set in the service config.
const RemovedRepoModelReason = "removed: models are chosen only in the service config (claude.*, review.routing); see docs/configuration.md"

// removedServiceKeys of the service config (config.yaml).
var removedServiceKeys = []string{
	"sandbox.setup_timeout",
	"sandbox.allow_unsandboxed_bash",
	"review.default_tools",
}

// removedRepoKeys of .reviewbot.yaml.
var removedRepoKeys = []string{
	"setup",
	"tools",
	"compat.commands",
	"routing",
}

// removedKeyReasons overrides RemovedKeyReason for some keys.
var removedKeyReasons = map[string]string{
	"compat.commands": RemovedRepoCommandsReason,
	"routing":         RemovedRepoModelReason,
}

// removedRepoKeyReason returns why a .reviewbot.yaml key path ("setup",
// "compat.commands", "lint.custom") no longer exists; ok=false for a key
// that was never there (ValidateRepo reports it as unknown).
func removedRepoKeyReason(path string) (reason string, ok bool) {
	if slices.Contains(removedRepoKeys, path) {
		if r, ok := removedKeyReasons[path]; ok {
			return r, true
		}
		return RemovedKeyReason, true
	}
	if k, ok := strings.CutPrefix(path, "lint."); ok && slices.Contains(removedRepoLintKeys, k) {
		return LintRemovedHint, true
	}
	return "", false
}

// ErrRemovedKey is wrapped by the error of checkRemovedKeys.
var ErrRemovedKey = errors.New("removed configuration key")

// checkRemovedKeys returns an error naming every removed key present in the
// YAML document b (whatever its value, null included).
func checkRemovedKeys(b []byte, keys []string) error {
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil || doc == nil {
		return nil // the typed parse reports syntax errors
	}
	var found []string
	for _, k := range keys {
		if hasKeyPath(doc, strings.Split(k, ".")) {
			found = append(found, k)
		}
	}
	if len(found) == 0 {
		return nil
	}
	return &removedKeyError{keys: found}
}

// removedKeyError reads "sandbox.setup_timeout, review.default_tools:
// removed: the review model …" and matches ErrRemovedKey. Every removed
// service key shares RemovedKeyReason (the keys with their own reason,
// removedKeyReasons, are .reviewbot.yaml keys: see removedRepoKeyReason).
type removedKeyError struct{ keys []string }

func (e *removedKeyError) Error() string {
	return strings.Join(e.keys, ", ") + ": " + RemovedKeyReason
}

func (e *removedKeyError) Is(target error) bool { return target == ErrRemovedKey }

func hasKeyPath(m map[string]any, path []string) bool {
	v, ok := m[path[0]]
	if !ok {
		return false
	}
	if len(path) == 1 {
		return true
	}
	sub, ok := v.(map[string]any)
	return ok && hasKeyPath(sub, path[1:])
}
