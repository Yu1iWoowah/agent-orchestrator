//go:build !unix

package integration

import "os"

const sourceOpenFlags = os.O_RDONLY
