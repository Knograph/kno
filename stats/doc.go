// Package stats owns everything Kno reports as a number: repeated trials,
// confidence intervals, the dev/holdout split, and redundancy detection.
//
// Statistical honesty is a feature, not a nicety. No delta is reported without
// its confidence interval, no selection happens without dev/holdout
// separation, and nothing reads the holdout before Validate. These are
// enforced by property tests and a holdout canary — do not weaken them to make
// something pass.
package stats
