package cmd

import "github.com/samsaffron/term-llm/internal/app/reload"

// Only the fallback token comes from private reload state. Explicit flags and
// the user's public environment keep their normal precedence.
func restoreOrGenerateServeToken() (string, error) {
	return restoreOrGenerateServeTokenWith(reload.ServeToken())
}

func restoreOrGenerateServeTokenWith(token string) (string, error) {
	if token != "" {
		return token, nil
	}
	return generateServeToken()
}
