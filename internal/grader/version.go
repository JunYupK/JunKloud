package grader

// GraderVersion identifies the grading semantics. A problem's validation
// certificate (author.CertificateMatches) is tied to it, so bumping it makes
// every certificate stale and every problem must be validated again before it
// is served.
//
// Bump rule: increase the number in the same commit as any change that can
// make an unchanged problem, graded in an unchanged environment, produce a
// different status, score, ordering of prerequisites or ERROR/FAIL
// classification. That covers:
//
//   - a checker's decision rules (what PASS, FAIL and ERROR mean for a kind),
//     its accepted Args or its comparison semantics;
//   - the engine's scoring, prerequisite, timeout-default or parallelism
//     rules;
//   - the fault classification of pods, pulls and probes;
//   - the probe, tool or runner behaviour a verdict depends on.
//
// Do not bump it for refactors, tests, log or Reason wording, evidence layout
// or performance changes that cannot alter a verdict. When in doubt, bump:
// a needless bump costs one re-validation, a missed one lets a certificate
// vouch for behaviour it never saw.
const GraderVersion = "grader-4"
