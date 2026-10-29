package repo

// The in-memory state of the snapshots: the current one per repository, the
// runs using each snapshot dir, and the removal of superseded ones.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type snapState struct {
	branch    string
	sha       string
	dir       string // current snapshot ("" if none)
	size      int64
	fetchedAt time.Time
	err       error // last refresh error, returned until the next refresh
	errBranch string
	tooLarge  string // sha known to exceed MaxBytes (not exported again)
}

type snapshotStore struct {
	dir string
	now func() time.Time

	mu    sync.Mutex
	repos map[string]*snapState
	refs  map[string]int // snapshot dir -> runs using it
}

// snapshotFormat marks a snapshots directory whose exports ignore the
// repositories' .gitattributes (attributes.go). Snapshots kept from an older
// version (export-ignore applied, export-subst expanded) are dropped at
// startup instead of being reused for their SHA.
const snapshotFormat = ".format-raw-attributes"

func newSnapshotStore(dir string) (*snapshotStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	_, ferr := os.Stat(filepath.Join(dir, snapshotFormat))
	stale := ferr != nil
	// Leftovers of a crash: unfinished exports and half-deleted trees.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.Name() == snapshotFormat {
				continue
			}
			if stale || strings.HasPrefix(e.Name(), ".tmp-") || strings.HasPrefix(e.Name(), ".trash-") {
				_ = os.RemoveAll(filepath.Join(dir, e.Name()))
			}
		}
	}
	if stale {
		if err := os.WriteFile(filepath.Join(dir, snapshotFormat), nil, 0o644); err != nil {
			return nil, err
		}
	}
	return &snapshotStore{dir: dir, now: time.Now, repos: map[string]*snapState{}, refs: map[string]int{}}, nil
}

// cached returns the current snapshot (or the last error) when the repo was
// refreshed less than interval ago and the branch did not change.
func (st *snapshotStore) cached(p, branch string, interval time.Duration) (*Snapshot, error, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	rs, ok := st.repos[p]
	if !ok || interval <= 0 || rs.fetchedAt.IsZero() || st.now().Sub(rs.fetchedAt) >= interval {
		return nil, nil, false
	}
	if rs.err != nil {
		if branch != rs.errBranch {
			return nil, nil, false
		}
		return nil, rs.err, true
	}
	if branch != "" && rs.branch != "" && branch != rs.branch {
		return nil, nil, false
	}
	if rs.dir == "" {
		return nil, nil, false
	}
	if _, err := os.Stat(rs.dir); err != nil {
		return nil, nil, false
	}
	return st.acquireLocked(p, rs), nil, true
}

func (st *snapshotStore) state(p string) *snapState {
	rs, ok := st.repos[p]
	if !ok {
		rs = &snapState{}
		st.repos[p] = rs
	}
	return rs
}

func (st *snapshotStore) acquireLocked(p string, rs *snapState) *Snapshot {
	st.refs[rs.dir]++
	return &Snapshot{Path: p, Branch: rs.branch, SHA: rs.sha, Dir: rs.dir, Size: rs.size, store: st}
}

func (st *snapshotStore) release(p, dir string) {
	st.mu.Lock()
	st.refs[dir]--
	var trash string
	if st.refs[dir] <= 0 {
		delete(st.refs, dir)
		if rs, ok := st.repos[p]; !ok || rs.dir != dir {
			trash = st.trashLocked(dir)
		}
	}
	st.mu.Unlock()
	if trash != "" {
		_ = os.RemoveAll(trash)
	}
}

// trashLocked atomically moves dir out of the way (so a new snapshot of the
// same SHA can never pick up a half-deleted tree) and returns where it went.
func (st *snapshotStore) trashLocked(dir string) string {
	name, err := randomName()
	if err != nil {
		return ""
	}
	trash := filepath.Join(st.dir, ".trash-"+name)
	if err := os.Rename(dir, trash); err != nil {
		return ""
	}
	return trash
}

// cleanupLocked removes snapshots of project prefix other than keep that no
// run uses. Returns the trash dirs to delete outside the lock.
func (st *snapshotStore) cleanupLocked(prefix, keep string) []string {
	entries, err := os.ReadDir(st.dir)
	if err != nil {
		return nil
	}
	var trash []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		d := filepath.Join(st.dir, e.Name())
		if d == keep || st.refs[d] > 0 {
			continue
		}
		if t := st.trashLocked(d); t != "" {
			trash = append(trash, t)
		}
	}
	return trash
}

// knownSize returns the remembered size of a snapshot, or measures it (a
// snapshot left from before a restart).
func (st *snapshotStore) knownSize(p, sha, dir string) int64 {
	st.mu.Lock()
	if rs, ok := st.repos[p]; ok && rs.sha == sha && rs.dir == dir {
		st.mu.Unlock()
		return rs.size
	}
	st.mu.Unlock()
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}
