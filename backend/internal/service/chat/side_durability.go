package chat

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/attachmentstore"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type durableSideContent struct {
	RunID       string
	Record      SideRecoveryRecord
	Attachments map[string]string
}

func (m *memorySideStore) snapshotLocked(id string) (durableSideContent, error) {
	for _, record := range m.exportLocked("") {
		if record.Side.ID == id {
			return durableSideContent{RunID: record.Side.AppRunID, Record: record, Attachments: map[string]string{}}, nil
		}
	}
	return durableSideContent{}, ErrSideClosed
}

func (m *memorySideStore) contentLocked(id string) (json.RawMessage, error) {
	saved, err := m.snapshotLocked(id)
	if err != nil {
		return nil, err
	}
	for i := range saved.Record.Turns {
		turn := &saved.Record.Turns[i]
		turn.Content = append([]domain.SideContent(nil), turn.Content...)
		for j := range turn.Content {
			item := &turn.Content[j]
			if item.Data == "" {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(item.Data)
			if err != nil {
				return nil, err
			}
			name := fmt.Sprintf("image-%x.img", sha256.Sum256(data))
			if err := m.attachments.PutCanonical(context.Background(), domain.SessionID(saved.Record.ProviderHostID), name, data); err != nil && !errors.Is(err, attachmentstore.ErrExists) {
				return nil, err
			}
			saved.Attachments[fmt.Sprintf("%s:%d", turn.Turn.ID, j)] = name
			item.Data = ""
		}
	}
	return json.Marshal(saved)
}

// finishWrite runs under mu. A failed durable write cannot become an accepted
// memory-only edit; restore the previous projection before returning its error.
func (m *memorySideStore) finishWrite(id string) func(*error) {
	if !m.protected[id] {
		return func(*error) {}
	}
	snapshot, snapshotErr := m.snapshotLocked(id)
	before, marshalErr := json.Marshal(snapshot)
	snapshotErr = errors.Join(snapshotErr, marshalErr)
	return func(result *error) {
		if snapshotErr != nil {
			*result = errors.Join(*result, snapshotErr)
			return
		}
		raw, err := m.contentLocked(id)
		if err == nil && *result == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err = m.durable.SaveSideCompactionContent(ctx, id, raw)
			cancel()
		}
		if err != nil || *result != nil {
			*result = errors.Join(*result, err, m.restoreContentLocked(before))
		}
	}
}

func (m *memorySideStore) restoreContentLocked(raw json.RawMessage) error {
	var saved durableSideContent
	if err := json.Unmarshal(raw, &saved); err != nil {
		return err
	}
	r := saved.Record
	for i := range r.Turns {
		for j := range r.Turns[i].Content {
			name := saved.Attachments[fmt.Sprintf("%s:%d", r.Turns[i].Turn.ID, j)]
			if name == "" {
				continue
			}
			file, _, err := m.attachments.Open(context.Background(), domain.SessionID(r.ProviderHostID), name)
			if err != nil {
				return err
			}
			data, err := io.ReadAll(file)
			closeErr := file.Close()
			if err := errors.Join(err, closeErr); err != nil {
				return err
			}
			r.Turns[i].Content[j].Data = base64.StdEncoding.EncodeToString(data)
		}
	}
	projection := newMemorySideStore()
	projection.runID = saved.RunID
	if _, err := projection.recover(saved.RunID, []SideRecoveryRecord{r}, time.Now()); err != nil {
		return err
	}
	id := r.Side.ID
	side := projection.sides[id]
	side.State, side.ErrorMessage, side.UpdatedAt = r.Side.State, r.Side.ErrorMessage, r.Side.UpdatedAt
	m.sides[id] = side
	m.turns[id], m.messages[id], m.activities[id], m.drafts[id] = projection.turns[id], projection.messages[id], projection.activities[id], projection.drafts[id]
	for i, item := range r.Turns {
		m.turns[id][i].State, m.turns[id][i].ErrorMessage, m.turns[id][i].CompletedAt = item.Turn.State, item.Turn.ErrorMessage, item.Turn.CompletedAt
	}
	if m.compactions == nil {
		m.compactions, m.compactionEpochs, m.unverified = map[string]string{}, map[string]string{}, map[string]bool{}
	}
	m.compactions[id], m.compactionEpochs[id], m.unverified[id] = r.CompactionID, r.CompactionEpoch, true
	return nil
}

func (m *memorySideStore) loadCompactions(ctx context.Context) error {
	if m.durable == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, err := m.durable.SideCompactions(ctx, false)
	if err != nil {
		return err
	}
	if m.protected == nil {
		m.protected = map[string]bool{}
	}
	for _, row := range rows {
		if row.Closed {
			m.closed[row.SideID] = true
		}
		if !row.Unresolved() || m.protected[row.SideID] {
			continue
		}
		if err := m.restoreContentLocked(row.Content); err != nil {
			return err
		}
		m.protected[row.SideID] = true
	}
	return nil
}

// ExplicitlyCloseSideCompaction is called only at the user close endpoint.
func (s *Service) ExplicitlyCloseSideCompaction(ctx context.Context, session domain.SessionID, id string) error {
	if s.sides == nil {
		return ErrSideUnavailable
	}
	if _, err := s.sides.ownedSide(ctx, session, id); err != nil {
		return err
	}
	store := s.sides.store.(*memorySideStore)
	if store.durable == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.durable.SideCompactions(ctx, true)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.SideID == id {
			if err := store.durable.TransitionSideCompaction(ctx, row.ID, row.Epoch, row.State, row.Historical, true); err != nil {
				return err
			}
		}
	}
	delete(store.protected, id)
	return nil
}
