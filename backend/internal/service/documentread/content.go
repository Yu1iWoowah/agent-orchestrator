package documentread

import (
	"context"
	"io"
	"os"

	"github.com/aoagents/agent-orchestrator/backend/internal/attachmentstore"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// Content holds bytes read through a newly validated managed source.
type Content struct {
	Document Resolved
	Bytes    []byte
}

// Read serves bounded bytes without render bootstraps or filesystem path authorization from the caller.
func (s *Service) Read(ctx context.Context, input Input, maxBytes int64, expected string) (Content, error) {
	if maxBytes < 1 || maxBytes > attachmentstore.MaxFileBytes {
		return Content{}, apierr.Invalid("DOCUMENT_LIMIT_INVALID", "A bounded document read is required", nil)
	}
	document, e := s.Resolve(ctx, input)
	if e != nil {
		return Content{}, e
	}
	if expected != "" && expected != document.Fingerprint {
		return Content{}, changed()
	}
	rootPath, p, e := s.location(ctx, document.Target.value)
	if e != nil {
		return Content{}, e
	}
	root, e := s.openRoot(rootPath)
	if e != nil {
		return Content{}, unavailable()
	}
	defer func() { _ = root.Close() }()
	file, before, e := openDocument(root, p)
	if e != nil {
		return Content{}, e
	}
	defer func() { _ = file.Close() }()
	if fingerprint(before) != document.Fingerprint {
		return Content{}, changed()
	}
	if before.Size() > maxBytes {
		return Content{}, tooLarge()
	}
	if e := s.validateRoot(ctx, root, rootPath); e != nil {
		return Content{}, e
	}
	data, e := io.ReadAll(io.LimitReader(contentReader{ctx, file}, maxBytes+1))
	if e != nil {
		return Content{}, e
	}
	if int64(len(data)) > maxBytes {
		return Content{}, tooLarge()
	}
	after, e := file.Stat()
	if e != nil || !os.SameFile(before, after) || fingerprint(before) != fingerprint(after) {
		return Content{}, changed()
	}
	current, e := documentInfo(root, p)
	if e != nil || !os.SameFile(after, current) || fingerprint(after) != fingerprint(current) {
		return Content{}, changed()
	}
	if e := s.validateRoot(ctx, root, rootPath); e != nil {
		return Content{}, e
	}
	return Content{Document: document, Bytes: data}, nil
}
func changed() error {
	return apierr.Conflict("DOCUMENT_CHANGED", "The document changed; refresh before reading", nil)
}
func tooLarge() error {
	return apierr.Invalid("DOCUMENT_TOO_LARGE", "The document exceeds the read limit", nil)
}

type contentReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contentReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.reader.Read(p)
}
