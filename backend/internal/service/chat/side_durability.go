package chat

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/attachmentstore"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type durableSideContent struct {
	RunID       string
	Record      SideRecoveryRecord
	Attachments map[string]string
	DraftImages map[string]string
}

func (m *memorySideStore) snapshotLocked(id string) (durableSideContent, error) {
	for _, record := range m.exportLocked("", id) {
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
			name, err := m.storeImage(saved.Record.ProviderHostID, item.Data)
			if err != nil {
				return nil, err
			}
			saved.Attachments[fmt.Sprintf("%s:%d", turn.Turn.ID, j)] = name
			item.Data = ""
		}
	}
	var draft struct {
		Version                 int
		Attachments, References []json.RawMessage
	}
	if json.Unmarshal([]byte(saved.Record.Draft), &draft) == nil && draft.Version == 1 && draft.Attachments != nil && draft.References != nil {
		saved.DraftImages = map[string]string{}
		for _, raw := range draftImageData([]byte(saved.Record.Draft)) {
			var data string
			if err := json.Unmarshal(raw, &data); err != nil {
				return nil, err
			}
			name, err := m.storeImage(saved.Record.ProviderHostID, data)
			if err != nil {
				return nil, err
			}
			marker := ""
			for marker == "" || strings.Contains(saved.Record.Draft, marker) {
				marker = `"ao-side-image:` + uuid.NewString() + `"`
			}
			saved.Record.Draft = strings.ReplaceAll(saved.Record.Draft, string(raw), marker)
			saved.DraftImages[marker] = name
		}
	}
	return json.Marshal(saved)
}

// draftImageData finds image request bodies retained by pending delivery,
// command and handoff receipts inside the serialized side composer draft.
func draftImageData(raw json.RawMessage) []json.RawMessage {
	var fields map[string]json.RawMessage
	var children []json.RawMessage
	var out []json.RawMessage
	if json.Unmarshal(raw, &fields) == nil {
		var mime, data string
		if json.Unmarshal(fields["mimeType"], &mime) == nil && strings.HasPrefix(mime, "image/") && json.Unmarshal(fields["data"], &data) == nil && data != "" {
			out = append(out, fields["data"])
		}
		for _, child := range fields {
			children = append(children, child)
		}
	} else {
		_ = json.Unmarshal(raw, &children)
	}
	for _, child := range children {
		out = append(out, draftImageData(child)...)
	}
	return out
}

func (m *memorySideStore) storeImage(hostID, encoded string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("image-%x.img", sha256.Sum256(data))
	if err := m.attachments.PutCanonical(context.Background(), domain.SessionID(hostID), name, data); err != nil && !errors.Is(err, attachmentstore.ErrExists) {
		return "", err
	}
	return name, nil
}

func (m *memorySideStore) readImage(hostID, name string) (string, error) {
	file, _, err := m.attachments.Open(context.Background(), domain.SessionID(hostID), name)
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(file)
	if err := errors.Join(err, file.Close()); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
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
		if *result != nil {
			return
		}
		if snapshotErr != nil {
			*result = errors.Join(*result, snapshotErr)
			return
		}
		raw, err := m.contentLocked(id)
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err = m.durable.SaveSideCompactionContent(ctx, id, raw)
			cancel()
		}
		if err != nil {
			*result = errors.Join(err, m.restoreContentLocked(before))
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
			data, err := m.readImage(r.ProviderHostID, name)
			if err != nil {
				return err
			}
			r.Turns[i].Content[j].Data = data
		}
	}
	for marker, name := range saved.DraftImages {
		data, err := m.readImage(r.ProviderHostID, name)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		r.Draft = strings.ReplaceAll(r.Draft, marker, string(raw))
	}
	projection := newMemorySideStore()
	projection.runID = saved.RunID
	if _, err := projection.recover(saved.RunID, []SideRecoveryRecord{r}, time.Now()); err != nil {
		return err
	}
	id := r.Side.ID
	side := projection.sides[id]
	side.State, side.ErrorMessage, side.UpdatedAt, side.RecreateRequired = r.Side.State, r.Side.ErrorMessage, r.Side.UpdatedAt, r.Side.RecreateRequired
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
			delete(m.protected, row.SideID)
		}
		if !row.Unresolved() || m.protected[row.SideID] {
			continue
		}
		m.protected[row.SideID] = true
		if err := m.restoreContentLocked(row.Content); err != nil {
			m.log.Warn("side compaction content unavailable", "side", row.SideID, "code", "CHAT_SIDE_COMPACTION_RESTORE_FAILED", "error", err)
		}
	}
	for _, row := range rows {
		if !m.protected[row.SideID] && row.ProviderHostID != "" {
			if err := m.attachments.RemoveSession(ctx, domain.SessionID(row.ProviderHostID)); err != nil {
				m.log.Warn("settled side attachment cleanup failed", "side", row.SideID, "code", "CHAT_SIDE_ATTACHMENTS_CLEANUP_FAILED", "error", err)
			}
		}
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
	return s.closeSideCompactions(ctx, id)
}

func (s *Service) closeSideCompactions(ctx context.Context, id string) error {
	store, ok := s.sides.store.(*memorySideStore)
	if !ok {
		return ErrSideUnavailable
	}
	if store.durable == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.durable.SideCompactions(ctx, false)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.SideID == id && !row.Closed {
			if err := store.durable.TransitionSideCompaction(ctx, row.ID, row.Epoch, row.State, row.Historical, true); err != nil {
				return err
			}
		}
	}
	delete(store.protected, id)
	return nil
}

func (m *sideManager) removeSideAttachments(ctx context.Context, side domain.SideConversation) {
	if store, ok := m.store.(*memorySideStore); ok && store.attachments != nil {
		if err := store.attachments.RemoveSession(ctx, domain.SessionID(side.ProviderHostID)); err != nil {
			m.service.log.Warn("side attachment cleanup failed", "side", side.ID, "code", "CHAT_SIDE_ATTACHMENTS_CLEANUP_FAILED", "error", err)
		}
	}
}
