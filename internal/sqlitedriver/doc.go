// Package sqlitedriver registers the platform's database/sql "sqlite" driver.
// Import it for side effects instead of importing a driver directly.
//
// Native builds use modernc.org/sqlite. The browser demo builds for js/wasm and
// never opens a SQLite database; it still needs the session/memory packages to
// compile, so the WebAssembly build registers no driver and any attempted
// Open fails loudly at runtime.
package sqlitedriver
