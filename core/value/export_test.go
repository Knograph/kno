package value

// MinDetectableHarmFor exposes the harm bound to the package's external tests.
//
// Exported through a _test.go file rather than the package surface: the bound
// is an internal detail of routing, and the gate it feeds (ControlUnderpowered)
// is what callers actually read. Tests need the raw number to pin where that
// gate crosses; nothing else does.
func MinDetectableHarmFor(m int) float64 { return minDetectableHarm(m) }
