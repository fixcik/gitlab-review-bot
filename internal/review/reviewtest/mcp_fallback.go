package reviewtest

// NewHead moves the MR to a new head after the reviewed one (so the next
// review runs Claude again).
func NewHead(h *Harness, sha string) {
	h.Repos.Ancestors[h.GL.MR.DiffRefs.HeadSHA] = true
	h.GL.MR.DiffRefs.HeadSHA = sha
}
