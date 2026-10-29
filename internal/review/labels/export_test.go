package labels

// Unexported names the external tests (package labels_test) use.

// Tri is tri.
type Tri = tri

// TriUnknown, TriNo and TriYes are the tri values.
const (
	TriUnknown = triUnknown
	TriNo      = triNo
	TriYes     = triYes
)

// Inputs is inputs.
type Inputs = inputs

// NewInputs returns the rule inputs of the files the MR changes; known
// false when they could not be listed.
func NewInputs(files []string, known bool) *Inputs {
	return &inputs{files: files, filesKnown: known}
}

// Facts is mrFacts.
var Facts = mrFacts

// IsTestFile is isTestFile; IsCodeFile is isCodeFile.
var (
	IsTestFile = isTestFile
	IsCodeFile = isCodeFile
)

// WithoutOverrideLabel is withoutOverrideLabel.
var WithoutOverrideLabel = withoutOverrideLabel
