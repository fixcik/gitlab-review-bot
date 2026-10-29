// Package reviewtest is the test harness of internal/review: exported fakes
// of the reviewer's collaborators (GitLab, repositories, Claude runner,
// store, limiter, diff index, compat checker) and Harness, a whole
// review.Reviewer built from them through review.New, the constructor
// production uses.
//
// The orchestrator's tests (package review_test) and the feature packages'
// external tests (package pipeline_test, verify_test, …) share it; it must
// not be imported by non-test code. Typical use:
//
//	h := reviewtest.New(t)
//	h.Deps.Lint = &reviewtest.FakeLint{}      // collaborators: before the first R
//	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
//	err := h.R().Handle(ctx, reviewtest.AutoJob())
//
// Collaborators are set through h.Deps (review.Deps) before the reviewer is
// built (the first h.R(); later changes panic, Reset drops the reviewer).
// h.Env() changes the running reviewer's shared env (a limiter between two
// jobs); h.Internals() reaches one feature of that reviewer
// (review.Internals, test support only).
//
// Feature tests that need a helper of another feature's tests find it here
// too (FakeLint, FakeTasks, FakeSysmap, …); the files are named after the
// test files the helpers came from.
package reviewtest
