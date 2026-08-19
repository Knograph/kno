// Package schema holds invariant tests over the generated kno.v1 contract.
//
// These live outside gen/ because gen/ is generated output that nobody should
// hand-edit, and outside core/ because they test the schema itself rather than
// any engine behavior. They are the tests that would catch a proto change
// which compiles fine and is quietly wrong.
package schema
