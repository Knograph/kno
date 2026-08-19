// Package core is the Kno engine: the five pipeline stages (Baseline, Value,
// Select, Validate, Export) and the Ring-0 contracts every adapter implements.
//
// core imports nothing above it. cli, tui, and api are thin shells over
// identical core calls sharing one event stream — the open-core seam is a
// directory boundary, not a fork. A change that leaks an upward dependency
// into core is rejected regardless of quality; an import-boundary test
// enforces it mechanically.
//
// The Ring-0 interfaces (Agent, Evals, Pool, Goal, Tuner, Capable, and the
// injectors) land in M0c and carry a stability promise from 1.0.
package core
