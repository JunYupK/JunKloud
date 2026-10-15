// Package author proves that an authored problem package is sound before it
// is served (spec 7.1) and records a certificate tied to the problem content,
// the grader version and the environment versions.
//
// A Validator drives the SAME environment Manager and the SAME grader engine
// that practice and exam sessions use: it applies the problem's setup,
// solution and case actions to a fresh, JunKloud-owned namespace bundle,
// evaluates, resets and repeats, and compares every evaluation with what the
// problem declares. Nothing about a problem is judged by test-only code.
//
// Layout: actions.go dispatches model.Actions to the Manager, validate.go is
// the Validator, cleanup.go removes the temporary bundle, certificate.go
// defines certificates and the environment hash, compose.go wires a real
// Manager, store and grader engine together. The files that need the
// environment package are Linux-only, like that package.
package author
