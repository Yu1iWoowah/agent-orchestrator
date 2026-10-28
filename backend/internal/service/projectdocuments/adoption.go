package projectdocuments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// Adopt copies only the four approved living prefixes, retaining the legacy
// files and rules configuration. Pending state prevents serving partial copies.
func (s *Service) Adopt(ctx context.Context, id domain.ProjectID, in AdoptInput) (domain.ProjectDocumentAdoption, error) {
	unlock := s.lock(id)
	defer unlock()
	state, shared, _, err := s.state(ctx, id)
	if err != nil {
		return domain.ProjectDocumentAdoption{}, err
	}
	if _, err := s.validate(ctx, id, in.Actor, in.Lease, state, false); err != nil {
		return domain.ProjectDocumentAdoption{}, err
	}
	if prior := state.Adoption; prior != nil && prior.Complete {
		if prior.SourceSessionID != in.SourceSessionID {
			return *prior, conflict("PROJECT_DOCUMENT_ALREADY_ADOPTED", "Living files already have an adoption source")
		}
		return *prior, nil
	}
	source, ok, err := s.store.GetSession(ctx, in.SourceSessionID)
	if err != nil {
		return domain.ProjectDocumentAdoption{}, err
	}
	if !ok || source.ProjectID != id || source.Kind != domain.KindOrchestrator {
		return domain.ProjectDocumentAdoption{}, apierr.Invalid("PROJECT_DOCUMENT_ADOPTION_SOURCE_INVALID", "Source must be an orchestrator of this project", nil)
	}
	if strings.ContainsAny(string(source.ID), "/\\") || string(source.ID) == ".." {
		return domain.ProjectDocumentAdoption{}, apierr.Invalid("PROJECT_DOCUMENT_ADOPTION_SOURCE_INVALID", "Invalid source session id", nil)
	}
	legacy := filepath.Join(s.dataDir, "artifacts", string(source.ID))
	if source.Metadata.ArtifactDir != "" && filepath.Clean(source.Metadata.ArtifactDir) != legacy {
		return domain.ProjectDocumentAdoption{}, apierr.Invalid("PROJECT_DOCUMENT_ADOPTION_SOURCE_INVALID", "Legacy artifact root is not canonical", nil)
	}
	src, err := s.openRoot(legacy)
	if err != nil {
		return domain.ProjectDocumentAdoption{}, err
	}
	defer func() { _ = src.Close() }()
	rootFile, err := src.Open(".")
	if err != nil {
		return domain.ProjectDocumentAdoption{}, err
	}
	rootIdentity, err := sourceFileID(rootFile)
	_ = rootFile.Close()
	if err != nil {
		return domain.ProjectDocumentAdoption{}, err
	}
	entries, data, err := snapshotLiving(ctx, src)
	if err != nil {
		return domain.ProjectDocumentAdoption{}, err
	}
	diagnostics, pathMap, err := auditLivingLinks(id, legacy, data)
	if err != nil {
		return domain.ProjectDocumentAdoption{}, err
	}
	if prior := state.Adoption; prior != nil && (prior.SourceRootIdentity != rootIdentity || prior.SourceSessionID != source.ID || !slices.Equal(prior.Files, entries)) {
		return domain.ProjectDocumentAdoption{}, conflict("PROJECT_DOCUMENT_ADOPTION_SOURCE_CHANGED", "Pending adoption source changed; preserve it for inspection")
	}
	dst, err := s.openRoot(shared)
	if err != nil {
		return domain.ProjectDocumentAdoption{}, err
	}
	defer func() { _ = dst.Close() }()
	// Check every collision before copying any new path.
	for _, entry := range entries {
		b, e := readFile(dst, entry.Path)
		if errors.Is(e, fs.ErrNotExist) {
			continue
		}
		if e != nil {
			return domain.ProjectDocumentAdoption{}, e
		}
		if hash(b) != entry.Hash {
			return domain.ProjectDocumentAdoption{}, conflict("PROJECT_DOCUMENT_ADOPTION_CONFLICT", "A shared file has different content: "+entry.Path)
		}
	}
	if state.Adoption == nil {
		state.Adoption = &domain.ProjectDocumentAdoption{SourceSessionID: source.ID, SourceRootIdentity: rootIdentity, Files: entries}
		if err := s.save(ctx, id, &state); err != nil {
			return domain.ProjectDocumentAdoption{}, err
		}
	}
	for _, entry := range entries {
		b, e := readFile(dst, entry.Path)
		if e == nil && hash(b) == entry.Hash {
			continue
		}
		if !errors.Is(e, fs.ErrNotExist) {
			return domain.ProjectDocumentAdoption{}, e
		}
		if err := atomicWrite(ctx, dst, entry.Path, MissingHash, data[entry.Path], func(rename func() error) error {
			return s.publish(ctx, id, in.Actor, in.Lease, func(context.Context) error { return rename() })
		}); err != nil {
			return domain.ProjectDocumentAdoption{}, err
		}
	}
	after, _, err := snapshotLiving(ctx, src)
	if err != nil {
		return domain.ProjectDocumentAdoption{}, err
	}
	if !slices.Equal(entries, after) {
		return domain.ProjectDocumentAdoption{}, conflict("PROJECT_DOCUMENT_ADOPTION_SOURCE_CHANGED", "Legacy source changed during adoption")
	}

	// Reopen the named source root so replacing it cannot complete an old plan.
	current, err := s.openRoot(legacy)
	if err != nil {
		return domain.ProjectDocumentAdoption{}, err
	}
	currentFile, err := current.Open(".")
	_ = current.Close()
	if err != nil {
		return domain.ProjectDocumentAdoption{}, err
	}
	currentIdentity, err := sourceFileID(currentFile)
	_ = currentFile.Close()
	if err != nil {
		return domain.ProjectDocumentAdoption{}, err
	}
	if currentIdentity != rootIdentity {
		return domain.ProjectDocumentAdoption{}, conflict("PROJECT_DOCUMENT_ADOPTION_SOURCE_CHANGED", "Source root identity changed")
	}
	evidence := []adoptionFileEvidence{}
	for _, entry := range entries {
		b, e := readFile(dst, entry.Path)
		if e != nil {
			return domain.ProjectDocumentAdoption{}, e
		}
		if hash(b) != entry.Hash {
			return domain.ProjectDocumentAdoption{}, conflict("PROJECT_DOCUMENT_ADOPTION_CONFLICT", "Adopted bytes changed: "+entry.Path)
		}
		evidence = append(evidence, adoptionFileEvidence{ProjectDocumentEntry: entry, SourcePath: entry.Path, DestinationPath: entry.Path, SourceHash: entry.Hash, DestinationHash: hash(b)})
	}
	manifest, err := json.MarshalIndent(adoptionManifest{ProjectID: id, SourceSessionID: source.ID, SourceRoot: legacy, SourceRootIdentity: rootIdentity, Files: evidence, LegacyPathMap: pathMap, Diagnostics: diagnostics, Complete: true}, "", "  ")
	if err != nil {
		return domain.ProjectDocumentAdoption{}, err
	}
	base, err := os.OpenRoot(s.dataDir)
	if err != nil {
		return domain.ProjectDocumentAdoption{}, err
	}
	defer func() { _ = base.Close() }()
	manifestPath := path.Join("projects", projectKey(id), "orchestrator", "adoptions", string(source.ID), "manifest.json")
	if err := s.publish(ctx, id, in.Actor, in.Lease, func(publicationCtx context.Context) error {
		if err := publishManifest(publicationCtx, base, manifestPath, manifest); err != nil {
			return err
		}
		state.Adoption.Complete = true
		state.Adoption.ManifestHash = hash(manifest)
		return s.save(publicationCtx, id, &state)
	}); err != nil {
		return domain.ProjectDocumentAdoption{}, err
	}
	return *state.Adoption, nil
}
func snapshotLiving(ctx context.Context, src *os.Root) ([]domain.ProjectDocumentEntry, map[string][]byte, error) {
	entries := []domain.ProjectDocumentEntry{}
	data := map[string][]byte{}
	var total int64
	for _, prefix := range []string{"handoff", "rules", "backlog", "architecture"} {
		info, err := src.Lstat(prefix)
		if err != nil {
			return nil, nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, apierr.Invalid("PROJECT_DOCUMENT_UNSAFE_PATH", "Adoption prefix is not a regular directory", nil)
		}
		err = fs.WalkDir(src.FS(), prefix, func(p string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if e := ctx.Err(); e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return apierr.Invalid("PROJECT_DOCUMENT_UNSAFE_PATH", "Adoption contains a symlink", nil)
			}
			if d.IsDir() {
				return nil
			}
			file, e := openFile(src, p)
			if e != nil {
				return e
			}
			before, e := file.Stat()
			if e != nil {
				_ = file.Close()
				return e
			}
			identity, e := sourceFileID(file)
			if e != nil {
				_ = file.Close()
				return e
			}
			b, e := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
			after, statErr := file.Stat()
			_ = file.Close()
			if statErr != nil {
				return statErr
			}
			if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
				return conflict("PROJECT_DOCUMENT_ADOPTION_SOURCE_CHANGED", "Source changed while reading: "+p)
			}
			if len(b) > MaxFileBytes {
				return apierr.Invalid("PROJECT_DOCUMENT_ADOPTION_LIMIT", "Source file exceeds the copy limit", nil)
			}
			if e != nil {
				return e
			}
			total += int64(len(b))
			if len(entries) >= maxFiles || total > 64<<20 {
				return apierr.Invalid("PROJECT_DOCUMENT_ADOPTION_LIMIT", "Adoption exceeds the bounded copy limit", nil)
			}
			entries = append(entries, domain.ProjectDocumentEntry{Path: p, Hash: hash(b), Size: int64(len(b)), SourceIdentity: identity, Backup: isBackup(p)})
			data[p] = b
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return entries, data, nil
}
func publishManifest(ctx context.Context, root *os.Root, p string, b []byte) error {
	// Validate/create the daemon-owned parent separately from public file names;
	// safe project keys can contain percent-encoded bytes.
	if err := mkdirs(root, path.Dir(p)); err != nil {
		return err
	}
	parent, err := root.OpenRoot(path.Dir(p))
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	root = parent
	p = path.Base(p)
	existing, err := openFile(root, p)
	if err == nil {
		defer func() { _ = existing.Close() }()
		// Receipts can exceed the text preview limit. Compare their full digest,
		// reading one extra byte to reject a longer file without an unbounded read.
		digest := sha256.New()
		size, err := io.Copy(digest, io.LimitReader(existing, int64(len(b))+1))
		if err != nil {
			return err
		}
		if size == int64(len(b)) && hex.EncodeToString(digest.Sum(nil)) == hash(b) {
			return nil
		}
		return conflict("PROJECT_DOCUMENT_MANIFEST_CONFLICT", "Adoption manifest already contains different evidence")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := mkdirs(root, path.Dir(p)); err != nil {
		return err
	}
	token, err := secret()
	if err != nil {
		return err
	}
	tmp := path.Join(path.Dir(p), ".ao-manifest-"+token)
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tmp) }()
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
	// A hard link publishes completed bytes without replacing an existing name.
	return root.Link(tmp, p)
}
