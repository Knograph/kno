package jsonl

// TrimSpaceForTest exposes the record-level whitespace rule to the external
// test package, so every ASCII blank spelling can be pinned without going
// through a file write.
var TrimSpaceForTest = trimSpace
