package credentials

import (
	"fmt"
	"os"

	"github.com/samsaffron/term-llm/internal/fileprivacy"
)

// writeFileAtomic is retained as the credentials-package entry point so every
// application-owned credential file uses the shared private-file policy.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if perm != fileprivacy.FileMode {
		return fmt.Errorf("unsupported credential file mode %04o", perm)
	}
	return fileprivacy.WriteFileAtomic(path, data)
}
