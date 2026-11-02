package persistenthost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// DeliveryMetaKey is removed by the host before a request reaches the provider.
const DeliveryMetaKey = "aoDeliveryAttempt"

const maxDeliveryRecords = 4096
const maxDeliveryJournalBytes = 64 << 20

var errDuplicateDeliveryAttempt = errors.New("delivery attempt already reserved")

type deliveryWait struct {
	NativeTurn string
	AttemptID  string
	Kind       string
}

type deliveryRecord struct {
	NativeThread  string                     `json:"nativeThread,omitempty"`
	NativeSession string                     `json:"nativeSession,omitempty"`
	Receipt       domain.ChatDeliveryReceipt `json:"receipt"`
	Method        string                     `json:"method"`
	NativeTurn    string                     `json:"nativeTurn,omitempty"`
}

type deliveryJournal struct {
	// Native in-flight sets drain on response/terminal; never discard a live
	// correlation merely to cap a cache and permanently lose proof of quiescence.
	waiting    map[string]deliveryWait // native pending approvals/tools
	executions map[string]bool         // native turns without a matched reservation

	checkpointDirty bool // disk checkpoint can be repaired from authoritative memory
	mu              sync.Mutex
	epoch           string
	file            *os.File
	size            int64
	records         map[string]deliveryRecord
	requests        map[string]string
	unsettled       map[string]nativeDeliveryGap
	uncovered       map[string]string
}

func newDeliveryJournal(path, epoch string) (*deliveryJournal, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &deliveryJournal{waiting: map[string]deliveryWait{}, executions: map[string]bool{}, epoch: epoch, file: f, records: map[string]deliveryRecord{}, requests: map[string]string{}, uncovered: map[string]string{}, unsettled: map[string]nativeDeliveryGap{}}, nil
}

// compact keeps only live or unacknowledged evidence. The host never restores
// this file into a different epoch. A failed checkpoint never invalidates memory.
func (j *deliveryJournal) compact() error {
	if err := j.file.Truncate(0); err != nil {
		return err
	}
	if _, err := j.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	j.size = 0
	encoder := json.NewEncoder(j.file)
	for _, record := range j.records {
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	size, err := j.file.Seek(0, io.SeekCurrent)
	j.size = size
	if err != nil {
		return err
	}
	if err := j.file.Sync(); err != nil {
		return err
	}
	j.checkpointDirty = false
	return nil
}

func (j *deliveryJournal) append(r deliveryRecord) (resultErr error) {
	defer func() {
		if resultErr != nil {
			j.checkpointDirty = true
		}
	}()
	if j.checkpointDirty {
		return j.compact()
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if j.size+int64(len(b)) > maxDeliveryJournalBytes {
		if err := j.compact(); err != nil {
			return err
		}
	}
	n, err := j.file.Write(b)
	j.size += int64(n)
	if err != nil {
		return err
	}
	if n != len(b) {
		return io.ErrShortWrite
	}
	return j.file.Sync()
}

// prepare is called after ACP ID rewriting but before any native frame write.
func (j *deliveryJournal) prepare(frame []byte) ([]byte, error) {
	var e struct {
		ID     json.RawMessage            `json:"id"`
		Method string                     `json:"method"`
		Params map[string]json.RawMessage `json:"params"`
	}
	if json.Unmarshal(frame, &e) != nil || len(e.ID) == 0 {
		return frame, nil
	}
	if e.Method == "" {
		j.mu.Lock()
		delete(j.waiting, string(e.ID))
		j.mu.Unlock()
		return frame, nil
	}

	outbound := e.Method == "session/prompt" || e.Method == "turn/start" || e.Method == "turn/steer" || e.Method == "thread/compact/start" || e.Method == "thread/rollback" || e.Method == "thread/revert" || e.Method == "thread/fork"
	if !outbound {
		switch e.Method {
		case "initialize", "authenticate", "session/new", "session/load", "session/resume", "session/list",
			"session/set_mode", "session/set_model", "session/set_config_option",
			"thread/start", "thread/resume", "thread/read", "thread/list", "thread/name/set", "thread/unsubscribe",
			"thread/backgroundTerminals/list", "turn/interrupt", "session/cancel",
			"config/read", "model/list", "skills/list", "account/read", "account/rateLimits/read", "mcpServerStatus/list":
			// Initialization is covered by launch admission; settings by the attempt
			// checkpoint (or the enclosing prompt reservation). Reads/control add no input.
			return frame, nil
		default:
			// An unclassified native method must not disappear from the denominator
			// merely because its caller currently has no canonical delivery reservation.
			j.mu.Lock()
			j.trackUncovered(string(e.ID), e.Method, e.Params)
			j.mu.Unlock()
			return frame, nil
		}
	}
	var meta map[string]json.RawMessage
	_ = json.Unmarshal(e.Params["_meta"], &meta)
	var attempt string
	_ = json.Unmarshal(meta[DeliveryMetaKey], &attempt)
	// Strip private correlation even when the evidence journal is unavailable.
	delete(meta, DeliveryMetaKey)
	if len(meta) == 0 {
		delete(e.Params, "_meta")
	} else {
		e.Params["_meta"], _ = json.Marshal(meta)
	}
	var envelope map[string]json.RawMessage
	_ = json.Unmarshal(frame, &envelope)
	envelope["params"], _ = json.Marshal(e.Params)
	clean, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	clean = append(clean, '\n')
	j.mu.Lock()
	defer j.mu.Unlock()
	if attempt == "" {
		j.trackUncovered(string(e.ID), e.Method, e.Params)
		return clean, nil
	}
	if _, exists := j.records[attempt]; exists {
		return nil, errDuplicateDeliveryAttempt
	}
	if len(j.records) >= maxDeliveryRecords {
		var oldest string
		for key, r := range j.records {
			if r.Receipt.ExecutionSettled && (oldest == "" || r.Receipt.ObservedAt.Before(j.records[oldest].Receipt.ObservedAt)) {
				oldest = key
			}
		}
		if oldest != "" {
			delete(j.records, oldest)
		}
	}
	if len(j.records) >= maxDeliveryRecords || len(j.requests) >= maxDeliveryRecords {
		j.trackUncovered(string(e.ID), e.Method, e.Params)
		return clean, ports.ErrChatDeliveryUnverified
	}
	receipt := domain.ChatDeliveryReceipt{AttemptID: attempt, ProviderRequestID: rpcCorrelation(e.ID), ProviderEpoch: j.epoch, Disposition: domain.ChatDeliveryUnknown, Source: "incomplete", ObservedAt: time.Now().UTC()}
	var session, thread string
	_ = json.Unmarshal(e.Params["sessionId"], &session)
	_ = json.Unmarshal(e.Params["threadId"], &thread)
	safeReference := receipt
	safeReference.ProviderTurnID = session
	if safeReference.Validate() != nil {
		j.trackUncovered(string(e.ID), e.Method, e.Params)
		return clean, ports.ErrChatDeliveryUnverified
	}
	safeReference.ProviderTurnID = thread
	if safeReference.Validate() != nil {
		j.trackUncovered(string(e.ID), e.Method, e.Params)
		return clean, ports.ErrChatDeliveryUnverified
	}
	record := deliveryRecord{Receipt: receipt, Method: e.Method, NativeSession: session, NativeThread: thread}
	// Retain the correlation in memory even if its durable checkpoint fails.
	j.records[attempt], j.requests[string(e.ID)] = record, attempt
	return clean, j.append(record)
}

func rpcCorrelation(id json.RawMessage) string {
	var s string
	if json.Unmarshal(id, &s) == nil {
		return s
	}
	return string(id)
}

// observe sees original provider frames, before ACP replay or local events.
func (j *deliveryJournal) observe(frame []byte) error {
	var e struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
		Params struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
			TurnID    string `json:"turnId"`
			ThreadID  string `json:"threadId"`
			SessionID string `json:"sessionId"`
			Update    struct {
				Kind   string `json:"sessionUpdate"`
				ToolID string `json:"toolCallId"`
				Status string `json:"status"`
			} `json:"update"`
			Item struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"item"`
		} `json:"params"`
	}
	if json.Unmarshal(frame, &e) != nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if e.Method == "" && len(e.ID) > 0 {
		j.observeUncoveredResponse(string(e.ID), e.Result, e.Error)
		attempt, ok := j.requests[string(e.ID)]
		delete(j.requests, string(e.ID))
		if !ok {
			return nil
		}
		record := j.records[attempt]
		if len(e.Result) == 0 || string(e.Result) == "null" || len(e.Error) > 0 {
			return nil
		}
		var result struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(e.Result, &result)
		if record.Method == "turn/start" && result.Turn.ID == "" {
			return nil
		}
		if record.Method == "session/prompt" {
			var prompt struct {
				StopReason string `json:"stopReason"`
			}
			if json.Unmarshal(e.Result, &prompt) != nil || prompt.StopReason == "" {
				return nil
			}
		}
		record.NativeTurn = result.Turn.ID
		delete(j.executions, record.NativeTurn)
		record.Receipt.Disposition = domain.ChatDeliveryAccepted
		record.Receipt.Source = "provider_response"
		record.Receipt.Complete = true
		record.Receipt.ProviderTurnID = result.Turn.ID
		record.Receipt.ExecutionSettled = record.Method == "session/prompt"
		record.Receipt.ObservedAt = time.Now().UTC()
		record.Receipt.ProgressSequence++
		j.records[attempt] = record
		if record.Receipt.ExecutionSettled {
			j.clearWaiting(attempt, record.NativeTurn)
		}
		return j.append(record)
	}
	if e.Params.SessionID != "" && (e.Method == "session/update" || e.Method == "session/request_permission") {
		// Session broadcasts qualify only while one exact prompt RPC is active.
		attempt := j.activeACPAttempt(e.Params.SessionID)
		if attempt == "" {
			return nil
		}
		record := j.records[attempt]
		progress := "running"
		switch {
		case e.Method == "session/request_permission":
			progress = "approval"
			if len(e.ID) > 0 {
				j.waiting[string(e.ID)] = deliveryWait{AttemptID: attempt, Kind: progress}
			}
		case e.Params.Update.Kind == "tool_call" || e.Params.Update.Kind == "tool_call_update":
			key := "acp-tool:" + attempt + ":" + e.Params.Update.ToolID
			if e.Params.Update.Status == "completed" || e.Params.Update.Status == "failed" {
				delete(j.waiting, key)
			} else {
				progress = "tool"
				j.waiting[key] = deliveryWait{AttemptID: attempt, Kind: progress}
			}
		case e.Params.Update.Kind != "agent_message_chunk" && e.Params.Update.Kind != "agent_thought_chunk":
			return nil
		}
		record.Receipt.Progress = progress
		record.Receipt.ProgressSequence++
		record.Receipt.ObservedAt = time.Now().UTC()
		j.records[attempt] = record
		return nil
	}

	if e.Method != "" {
		native := e.Params.Turn.ID
		if native == "" {
			native = e.Params.TurnID
		}
		if native == "" {
			return nil
		}
		j.observeUncoveredEvent(e.Method, e.Params.ThreadID, native, e.Params.Item.Type)
		if e.Method == "turn/started" {
			j.executions[native] = true
		}
		if e.Method == "turn/completed" {
			delete(j.executions, native)
			j.clearWaiting("", native)
		}
		if strings.Contains(e.Method, "requestApproval") && len(e.ID) > 0 {
			j.waiting[string(e.ID)] = deliveryWait{NativeTurn: native, Kind: "approval"}
		}
		if e.Method == "item/started" && (e.Params.Item.Type == "commandExecution" || e.Params.Item.Type == "mcpToolCall") {
			j.waiting["item:"+e.Params.Item.ID] = deliveryWait{NativeTurn: native, Kind: "tool"}
		}
		if e.Method == "item/completed" {
			delete(j.waiting, "item:"+e.Params.Item.ID)
		}
		for attempt, record := range j.records {
			if record.NativeTurn == "" || record.NativeTurn != native || record.NativeThread != "" && e.Params.ThreadID != "" && record.NativeThread != e.Params.ThreadID || record.Receipt.Disposition != domain.ChatDeliveryAccepted {
				continue
			}
			delete(j.executions, native)
			record.Receipt.Source = "provider_event"
			record.Receipt.ObservedAt = time.Now().UTC()
			record.Receipt.ProgressSequence++
			if e.Method == "turn/completed" {
				record.Receipt.ExecutionSettled = true
			}
			if strings.Contains(e.Method, "requestApproval") {
				record.Receipt.Progress = "approval"
			} else if e.Method == "item/started" {
				record.Receipt.Progress = "tool"
			} else {
				record.Receipt.Progress = "running"
			}
			// Ordinary progress is already in the native replay stream. Checkpoint
			// only terminal evidence here; explicit custody flush saves the latest state.
			j.records[attempt] = record
			if e.Method == "turn/completed" {
				if err := j.append(record); err != nil {
					return err
				}
				j.clearWaiting(attempt, native)
			}
			j.records[attempt] = record
		}
	}

	return nil
}

// ACP notifications lack a request ID. Multiple active prompts are ambiguous;
// a completed/error request never borrows another prompt's session broadcasts.
func (j *deliveryJournal) activeACPAttempt(session string) string {
	active := ""
	for _, attempt := range j.requests {
		record := j.records[attempt]
		if record.Method == "session/prompt" && record.NativeSession == session && !record.Receipt.ExecutionSettled {
			if active != "" {
				return ""
			}
			active = attempt
		}
	}
	return active
}

func (j *deliveryJournal) clearWaiting(attempt, native string) {
	for key, wait := range j.waiting {
		if attempt != "" && wait.AttemptID == attempt || native != "" && wait.NativeTurn == native {
			delete(j.waiting, key)
		}
	}
}

// Acknowledge only evidence already committed by the daemon. Eviction never
// turns an absent receipt into proof of non-delivery.
func (j *deliveryJournal) acknowledge(keys []ports.ChatDeliveryLookup) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, key := range keys {
		record, ok := j.records[key.AttemptID]
		if !ok {
			continue
		}
		if key.ProviderEpoch != j.epoch || key.ProviderRequestID != record.Receipt.ProviderRequestID || !record.Receipt.ExecutionSettled {
			return ports.ErrChatDeliveryUnverified
		}
	}
	for _, key := range keys {
		delete(j.records, key.AttemptID)
	}
	return nil
}

func (j *deliveryJournal) query(lookups []ports.ChatDeliveryLookup, flush bool) ([]domain.ChatDeliveryReceipt, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if flush {
		if len(j.uncovered) > 0 || len(j.unsettled) > 0 || len(j.executions) > 0 {
			return nil, ports.ErrChatShutdownNotSettled
		}
		// Rewrite the latest memory snapshot, including progress and retired records.
		if err := j.compact(); err != nil {
			j.checkpointDirty = true
			return nil, ports.ErrChatDeliveryUnverified
		}
	}
	out := make([]domain.ChatDeliveryReceipt, 0, len(lookups))
	for _, lookup := range lookups {
		record, ok := j.records[lookup.AttemptID]
		if !ok || lookup.ProviderEpoch != "" && lookup.ProviderEpoch != j.epoch || lookup.ProviderRequestID != "" && lookup.ProviderRequestID != record.Receipt.ProviderRequestID {
			return nil, ports.ErrChatDeliveryUnverified
		}
		receipt := record.Receipt
		if receipt.Progress == "approval" || receipt.Progress == "tool" {
			receipt.Progress = "running"
		}
		active := record.Method != "session/prompt" || j.activeACPAttempt(record.NativeSession) == lookup.AttemptID
		for _, wait := range j.waiting {
			if active && !receipt.ExecutionSettled && (wait.AttemptID == lookup.AttemptID || record.NativeTurn != "" && wait.NativeTurn == record.NativeTurn) {
				if wait.Kind == "approval" || receipt.Progress != "approval" {
					receipt.Progress = wait.Kind
				}
			}
		}
		receipt.CustodyFlushed = flush

		out = append(out, receipt)
	}
	return out, nil
}

// DeliveryClient uses the existing authenticated host control endpoint.
type DeliveryClient struct{ descriptor Descriptor }

// QueryDeliveryAttempt reads only the original host correlation.
func (c *DeliveryClient) QueryDeliveryAttempt(ctx context.Context, key ports.ChatDeliveryLookup) (domain.ChatDeliveryReceipt, error) {
	receipts, err := c.call(ctx, []ports.ChatDeliveryLookup{key}, "delivery-query")
	if err != nil || len(receipts) != 1 {
		return domain.ChatDeliveryReceipt{}, errors.Join(ports.ErrChatDeliveryUnverified, err)
	}
	return receipts[0], nil
}

// FenceAndFlushOutbound confirms journal custody before transport detach.
func (c *DeliveryClient) FenceAndFlushOutbound(ctx context.Context, keys []ports.ChatDeliveryLookup) ([]domain.ChatDeliveryReceipt, error) {
	return c.call(ctx, keys, "delivery-flush")
}

// AcknowledgeDeliveryAttempt releases a settled receipt after durable consumption.
func (c *DeliveryClient) AcknowledgeDeliveryAttempt(ctx context.Context, key ports.ChatDeliveryLookup) error {
	_, err := c.call(ctx, []ports.ChatDeliveryLookup{key}, "delivery-ack")
	return err
}
func (c *DeliveryClient) call(ctx context.Context, keys []ports.ChatDeliveryLookup, action string) ([]domain.ChatDeliveryReceipt, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.descriptor.Address)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	finish := bindConnToContext(ctx, conn)
	if err := json.NewEncoder(conn).Encode(hello{Version: ProtocolVersion, Token: c.descriptor.Token, Action: action, Deliveries: keys}); err != nil {
		return nil, finish(err)
	}
	var response helloResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return nil, finish(err)
	}
	if err := finish(nil); err != nil {
		return nil, err
	}
	if !response.OK {
		return nil, ports.ErrChatDeliveryUnverified
	}
	return response.Deliveries, nil
}

func deliveryEpoch(d Descriptor) string {
	return "host-" + strconv.Itoa(d.PID) + "-" + strconv.FormatInt(d.StartedAt.UnixNano(), 10)
}

// flushReplay confirms the ACP replay boundary before reporting custody. It
// shares the relay mutex with ACK/reset and never runs after detaching a client.
func (h *host) flushReplay(ctx context.Context) error {
	if h.acp == nil {
		return nil
	}
	for !h.mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	defer h.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(h.acp.promptJournal.file.Sync(), h.acp.unpromptedJournal.file.Sync())
}
