// Package processinfo provides cycle-safe process predicates shared by tests.
// It intentionally depends only on the standard library and x/sys so provider
// tests can use it without importing the larger testutil harness.
package processinfo
