// Package passkeyauth provides application-neutral WebAuthn authentication
// primitives for first-party term-llm servers. It owns relying-party protocol
// handling, a versioned single-user credential store, browser sessions,
// ceremonies, and host-controlled enrollment grants.
//
// Callers supply product policy such as the public endpoint, relying-party and
// user display names, store location, HTTP routes, cookie names, and UI. The
// package deliberately contains no Hub routing, branding, or proxy behavior so
// serve variants can share one authentication engine.
package passkeyauth
