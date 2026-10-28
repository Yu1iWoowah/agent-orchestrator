package projectdocuments

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// Info locates shared documents independently of source session lifetime.
func (s *Service) Info(ctx context.Context, id domain.ProjectID) (Info, error) {
	unlock := s.lock(id)
	defer unlock()
	state, root, doc, err := s.state(ctx, id)
	if err != nil {
		return Info{}, err
	}
	r, err := s.openRoot(root)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Info{}, err
	}
	files := []domain.ProjectDocumentEntry{}
	if r != nil {
		defer func() { _ = r.Close() }()
	}
	if r != nil && (state.Adoption == nil || state.Adoption.Complete) {
		files, err = listFiles(ctx, r)
		if err != nil {
			return Info{}, err
		}
	}
	info := Info{SharedDir: root, DocumentPath: doc, Files: files, Adoption: state.Adoption}
	if state.Lease != nil {
		info.Lease = &LeaseView{SessionID: state.Lease.SessionID, Epoch: state.Lease.Epoch, Frozen: state.Lease.Frozen}
	}
	return info, nil
}
func (s *Service) Read(ctx context.Context, id domain.ProjectID, p string) (File, error) {
	if err := cleanPath(p); err != nil {
		return File{}, err
	}
	unlock := s.lock(id)
	defer unlock()
	state, root, _, err := s.state(ctx, id)
	if err != nil {
		return File{}, err
	}
	if state.Adoption != nil && !state.Adoption.Complete {
		return File{}, conflict("PROJECT_DOCUMENT_ADOPTION_PENDING", "Living file adoption is incomplete")
	}
	r, err := s.openRoot(root)
	if err != nil {
		return File{}, apierr.NotFound("PROJECT_DOCUMENT_NOT_FOUND", "Project document not found")
	}
	defer func() { _ = r.Close() }()
	b, err := readFile(r, p)
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, apierr.NotFound("PROJECT_DOCUMENT_NOT_FOUND", "Project document not found")
	}
	if err != nil {
		return File{}, err
	}
	return fileView(p, b), nil
}
func fileView(p string, b []byte) File {
	file := File{Path: p, Hash: hash(b), Size: int64(len(b)), Binary: !utf8.Valid(b) || strings.ContainsRune(string(b), 0)}
	if !file.Binary {
		file.Content = string(b)
	}
	return file
}
func (s *Service) Write(ctx context.Context, id domain.ProjectID, in WriteInput) (File, error) {
	if err := cleanPath(in.Path); err != nil {
		return File{}, err
	}
	if len(in.Content) > MaxFileBytes || !utf8.ValidString(in.Content) || strings.ContainsRune(in.Content, 0) {
		return File{}, apierr.Invalid("PROJECT_DOCUMENT_CONTENT_INVALID", "A bounded UTF-8 text document is required", nil)
	}
	unlock := s.lock(id)
	defer unlock()
	state, root, _, err := s.state(ctx, id)
	if err != nil {
		return File{}, err
	}
	if _, err := s.validate(ctx, id, in.Actor, in.Lease, state, false); err != nil {
		return File{}, err
	}
	if state.Adoption != nil && !state.Adoption.Complete {
		return File{}, conflict("PROJECT_DOCUMENT_ADOPTION_PENDING", "Finish adoption before writing living files")
	}
	r, err := s.openRoot(root)
	if err != nil {
		return File{}, err
	}
	defer func() { _ = r.Close() }()
	if err := atomicWrite(ctx, r, in.Path, in.ExpectedHash, []byte(in.Content), func(rename func() error) error {
		return s.publish(ctx, id, in.Actor, in.Lease, func(context.Context) error { return rename() })
	}); err != nil {
		return File{}, err
	}
	return fileView(in.Path, []byte(in.Content)), nil
}
func atomicWrite(ctx context.Context, r *os.Root, p, expected string, b []byte, publish func(func() error) error) error {
	if expected == "" {
		return apierr.Invalid("PROJECT_DOCUMENT_HASH_REQUIRED", "expectedHash is required; use missing for creation", nil)
	}
	if err := checkExpected(r, p, expected); err != nil {
		return err
	}
	if err := mkdirs(r, path.Dir(p)); err != nil {
		return err
	}
	token, err := secret()
	if err != nil {
		return err
	}
	tmp := path.Join(path.Dir(p), ".ao-write-"+token)
	f, err := r.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = r.Remove(tmp) }()
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkExpected(r, p, expected); err != nil {
		return err
	}
	return publish(func() error { return r.Rename(tmp, p) })
}

// Open returns a confined regular file for read-only streaming and downloads.
// The caller owns the returned descriptor; no session lifetime is required.
func (s *Service) Open(ctx context.Context, id domain.ProjectID, p string) (*os.File, error) {
	if err := cleanPath(p); err != nil {
		return nil, err
	}
	unlock := s.lock(id)
	defer unlock()
	state, root, _, err := s.state(ctx, id)
	if err != nil {
		return nil, err
	}
	if state.Adoption != nil && !state.Adoption.Complete {
		return nil, conflict("PROJECT_DOCUMENT_ADOPTION_PENDING", "Living file adoption is incomplete")
	}
	r, err := s.openRoot(root)
	if err != nil {
		return nil, apierr.NotFound("PROJECT_DOCUMENT_NOT_FOUND", "Project document not found")
	}
	defer func() { _ = r.Close() }()
	f, err := openFile(r, p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, apierr.NotFound("PROJECT_DOCUMENT_NOT_FOUND", "Project document not found")
	}
	return f, err
}

// ReadRoot locates an adopted shared tree without creating it or acquiring a writer lease.
// The read catalog performs its own confined traversal, independent of Info's legacy list cap.
func (s *Service) ReadRoot(ctx context.Context, id domain.ProjectID) (string, error) {
	unlock := s.lock(id)
	defer unlock()
	state, root, _, err := s.state(ctx, id)
	if err != nil {
		return "", err
	}
	if state.Adoption != nil && !state.Adoption.Complete {
		return "", conflict("PROJECT_DOCUMENT_ADOPTION_PENDING", "Living file adoption is incomplete")
	}
	return root, nil
}
