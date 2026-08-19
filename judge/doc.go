// Package judge scores outcomes and routes assets by mechanism, using typed
// LLM functions defined in judge/baml_src.
//
// Judges are the epistemic foundation, so they get explicit treatment: the
// judge model defaults to something other than the agent model (correlated
// blind spots are real), agreement against a human-labeled calibration set is
// reported before you trust a run, and every prompt lives where changing it is
// a reviewable diff.
package judge
