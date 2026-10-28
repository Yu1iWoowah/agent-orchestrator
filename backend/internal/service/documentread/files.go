package documentread

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func subdir(parent *os.Root, name string) (*os.Root, error) {
	before, e := parent.Lstat(name)
	if e != nil {
		return nil, e
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, unavailable()
	}
	child, e := parent.OpenRoot(name)
	if e != nil {
		return nil, e
	}
	opened, e := child.Stat(".")
	if e != nil {
		child.Close()
		return nil, e
	}
	after, e := parent.Lstat(name)
	if e != nil || !os.SameFile(before, opened) || !os.SameFile(before, after) {
		child.Close()
		return nil, unavailable()
	}
	return child, nil
}
func (s *Service) openRoot(full string) (*os.Root, error) {
	if s.dataDir == "" || !filepath.IsAbs(full) {
		return nil, unavailable()
	}
	rel, ok := within(s.dataDir, full)
	if !ok {
		return nil, unavailable()
	}
	info, e := os.Lstat(s.dataDir)
	if e != nil {
		return nil, e
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, unavailable()
	}
	root, e := os.OpenRoot(s.dataDir)
	if e != nil {
		return nil, e
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		next, e := subdir(root, part)
		root.Close()
		if e != nil {
			return nil, e
		}
		root = next
	}
	return root, nil
}
func (s *Service) metadata(ctx context.Context, root *os.Root, rootPath, p string) (os.FileInfo, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if !validPath(p) {
		return nil, invalid()
	}
	info, e := documentInfo(root, p)
	if e != nil {
		return nil, e
	}
	// The returned physical path must still identify the tree we opened.
	checked, e := s.openRoot(rootPath)
	if e != nil {
		return nil, unavailable()
	}
	defer checked.Close()
	original, e := root.Stat(".")
	if e != nil {
		return nil, unavailable()
	}
	now, e := checked.Stat(".")
	if e != nil || !os.SameFile(original, now) {
		return nil, unavailable()
	}
	actual, e := documentInfo(checked, p)
	if e != nil || !os.SameFile(info, actual) {
		return nil, unavailable()
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	return info, nil
}
func updatedAt(info os.FileInfo) *time.Time {
	t := info.ModTime().UTC()
	if t.IsZero() {
		return nil
	}
	return &t
}

func documentInfo(root *os.Root, p string) (os.FileInfo, error) {
	current := root
	var opened []*os.Root
	defer func() {
		for _, r := range opened {
			r.Close()
		}
	}()
	parts := strings.Split(p, "/")
	for _, part := range parts[:len(parts)-1] {
		next, e := subdir(current, part)
		if e != nil {
			return nil, unavailable()
		}
		opened = append(opened, next)
		current = next
	}
	name := parts[len(parts)-1]
	before, e := current.Lstat(name)
	if e != nil || !before.Mode().IsRegular() {
		return nil, unavailable()
	}
	file, e := openRegular(current, name)
	if e != nil {
		return nil, unavailable()
	}
	defer file.Close()
	info, e := file.Stat()
	if e != nil || !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return nil, unavailable()
	}
	after, e := current.Lstat(name)
	if e != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, after) {
		return nil, unavailable()
	}
	return info, nil
}
