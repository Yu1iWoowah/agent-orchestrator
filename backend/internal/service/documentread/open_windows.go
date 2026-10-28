package documentread

import "os"

func openRegular(root *os.Root, name string) (*os.File, error) { return root.Open(name) }
