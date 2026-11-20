package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionartifacts"
)

// keptArtifactStore records the artifacts a person keeps when they delete
// their session. It is optional so focused store fakes need not implement it.
type keptArtifactStore interface {
	KeepSessionArtifacts(ctx context.Context, kept domain.KeptArtifactSession) error
	ListKeptArtifactSessions(ctx context.Context) ([]domain.KeptArtifactSession, error)
	GetKeptArtifactSession(ctx context.Context, id domain.SessionID) (domain.KeptArtifactSession, bool, error)
	ListKeptArtifacts(ctx context.Context, id domain.SessionID) ([]domain.SessionArtifactRecord, error)
	RemoveKeptArtifact(ctx context.Context, id domain.SessionID, path string) error
	ForgetKeptArtifactSession(ctx context.Context, id domain.SessionID) error
}

// KeptArtifacts is the read-only artifact set of a deleted session.
type KeptArtifacts struct {
	SessionID domain.SessionID
	// ProjectID is the session's project, empty when it has none any more.
	ProjectID domain.ProjectID
	Title     string
	// At is when the session was deleted.
	At    time.Time
	Files []domain.SessionArtifactFile
}

// DeleteKeepingArtifacts deletes a terminated session the way Delete does but
// keeps its artifact files, view only, until they are deleted one by one. A
// session without artifact files is simply deleted.
func (s *Service) DeleteKeepingArtifacts(ctx context.Context, id domain.SessionID, cleanup bool) error {
	rec, found, err := s.store.GetSession(ctx, id)
	if err != nil {
		return toAPIError(err)
	}
	if !found {
		return apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	dir := sessionartifacts.Dir(s.dataDir, id)
	// Check the recorded location before inspecting the default directory.
	// Deleting the session would lose access to artifacts stored elsewhere.
	if stored := rec.Metadata.ArtifactDir; stored != "" && filepath.Clean(stored) != dir {
		return apierr.Invalid("ARTIFACT_DIR_UNSUPPORTED", "The session's artifacts are stored outside its current artifact directory.", nil)
	}
	hasFiles, err := sessionartifacts.HasFiles(ctx, dir)
	if err != nil {
		return fmt.Errorf("delete %s: check artifacts: %w", id, err)
	}
	if !hasFiles {
		return s.Delete(ctx, id, cleanup)
	}
	keeper, ok := s.store.(keptArtifactStore)
	deleter, canDelete := s.manager.(sessionDeleteCommander)
	if !ok || !canDelete {
		return apierr.NotImplemented("SESSION_DELETE_UNSUPPORTED", "Keeping artifacts is not supported by this daemon")
	}
	title := presentedTerminalTitle(rec, s.storedTerminalTitles(ctx, []domain.SessionID{id})[id])
	if err := keeper.KeepSessionArtifacts(ctx, domain.KeptArtifactSession{
		SessionID: id, ProjectID: rec.ProjectID, Title: cmp.Or(title, recordTitle(rec)), KeptAt: s.now(),
	}); err != nil {
		return err
	}
	if err := deleter.DeleteSession(ctx, id, cleanup); err != nil {
		return toAPIError(err)
	}
	s.cancelTitleRefinement(id)
	// Version history is a session's, and its rows went with it.
	if err := os.RemoveAll(filepath.Join(dir, sessionartifacts.VersionsDir)); err != nil && s.logger != nil {
		s.logger.Warn("delete version history of kept artifacts", "session", id, "err", err)
	}
	return nil
}

// forgetKeptArtifacts drops a kept record for a session whose files are gone.
func (s *Service) forgetKeptArtifacts(ctx context.Context, id domain.SessionID) {
	keeper, ok := s.store.(keptArtifactStore)
	if !ok {
		return
	}
	if err := keeper.ForgetKeptArtifactSession(ctx, id); err != nil && s.logger != nil {
		s.logger.Warn("forget kept artifacts", "session", id, "err", err)
	}
}

// recordTitle is a session's title without its terminal program's.
func recordTitle(rec domain.SessionRecord) string {
	return cmp.Or(rec.DisplayName, string(rec.IssueID), string(rec.ID))
}

// ListKeptArtifacts lists artifacts of deleted sessions, most recently deleted
// first. A set with no files left is left out.
func (s *Service) ListKeptArtifacts(ctx context.Context) ([]KeptArtifacts, error) {
	keeper, ok := s.store.(keptArtifactStore)
	if !ok {
		return nil, nil
	}
	kept, err := keeper.ListKeptArtifactSessions(ctx)
	if err != nil {
		return nil, err
	}
	var out []KeptArtifacts
	for _, k := range kept {
		files, err := s.keptFiles(ctx, keeper, k.SessionID)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("list kept artifacts", "session", k.SessionID, "err", err)
			}
			continue
		}
		if len(files) == 0 {
			continue
		}
		out = append(out, KeptArtifacts{SessionID: k.SessionID, ProjectID: k.ProjectID, Title: k.Title, At: k.KeptAt, Files: files})
	}
	return out, nil
}

// keptFiles lists the files kept with a deleted session, with their links.
func (s *Service) keptFiles(ctx context.Context, keeper keptArtifactStore, id domain.SessionID) ([]domain.SessionArtifactFile, error) {
	rows, err := keeper.ListKeptArtifacts(ctx, id)
	if err != nil {
		return nil, err
	}
	observation, err := sessionartifacts.Observe(ctx, sessionartifacts.Dir(s.dataDir, id), sessionartifacts.RegisteredPaths(rows))
	if err != nil {
		return nil, err
	}
	files := observation.Apply(rows)
	s.shareLinks.Apply(id, files)
	return files, nil
}

// ArtifactSession returns the session whose artifact files a request reads:
// the session itself, or, for a session deleted with its artifacts kept, a
// terminated stand-in holding only its artifact directory and files.
func (s *Service) ArtifactSession(ctx context.Context, id domain.SessionID) (sess domain.Session, kept bool, err error) {
	sess, err = s.Get(ctx, id)
	if err == nil || !isNotFound(err) {
		return sess, false, err
	}
	keeper, ok := s.store.(keptArtifactStore)
	if !ok {
		return domain.Session{}, false, err
	}
	record, found, lookupErr := keeper.GetKeptArtifactSession(ctx, id)
	if lookupErr != nil {
		return domain.Session{}, false, lookupErr
	}
	if !found {
		return domain.Session{}, false, err
	}
	files, filesErr := s.keptFiles(ctx, keeper, id)
	if filesErr != nil {
		return domain.Session{}, false, filesErr
	}
	stand := domain.Session{ArtifactFiles: files}
	stand.ID = id
	stand.ProjectID = record.ProjectID
	stand.DisplayName = record.Title
	stand.IsTerminated = true
	stand.Metadata.ArtifactDir = sessionartifacts.Dir(s.dataDir, id)
	return stand, true, nil
}

// ForgetKeptArtifact forgets a kept artifact whose file was just deleted.
// Once the session has no files left, its directory and record go too.
func (s *Service) ForgetKeptArtifact(ctx context.Context, id domain.SessionID, path string) error {
	keeper, ok := s.store.(keptArtifactStore)
	if !ok {
		return nil
	}
	if err := keeper.RemoveKeptArtifact(ctx, id, path); err != nil {
		return err
	}
	dir := sessionartifacts.Dir(s.dataDir, id)
	hasFiles, err := sessionartifacts.HasFiles(ctx, dir)
	if err != nil || hasFiles {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove kept artifact dir of %s: %w", id, err)
	}
	return keeper.ForgetKeptArtifactSession(ctx, id)
}

func isNotFound(err error) bool {
	var apiErr *apierr.Error
	return errors.As(err, &apiErr) && apiErr.Kind == apierr.KindNotFound
}
