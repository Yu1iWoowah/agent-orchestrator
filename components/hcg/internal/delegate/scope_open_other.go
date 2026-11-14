//go:build !unix

package delegate

import "os"

const sourceOpenFlags = os.O_RDONLY
