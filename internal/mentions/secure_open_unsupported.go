//go:build (js && wasm) || plan9

package mentions

func openSecureMentionRoot(string) (secureMentionRoot, error) {
	// os.Root is pathname-based on these platforms and documents TOCTOU or
	// rename limitations, so eager mention attachment reads fail closed.
	return nil, errSecureOpenUnavailable
}
