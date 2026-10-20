// Package sqlitedriver registers the platform's database/sql "sqlite" driver.
// Import it for side effects instead of importing a driver directly.
//
// Native builds use modernc.org/sqlite. This js/wasm build registers no driver,
// so opening a SQLite database fails with database/sql's "unknown driver"
// error. A host-supplied SQLite backend is outside this change.
package sqlitedriver
