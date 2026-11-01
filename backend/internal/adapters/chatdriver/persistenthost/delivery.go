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

type deliveryRecord struct {
	Receipt    domain.ChatDeliveryReceipt `json:"receipt"`
	Method     string                     `json:"method"`
	NativeTurn string                     `json:"nativeTurn,omitempty"`
}

type deliveryJournal struct {
	waiting    map[string]string // native request/item id -> native turn
	executions map[string]bool   // native turns without a matched reservation

	mu        sync.Mutex
	epoch     string
	file      *os.File
	size      int64
	records   map[string]deliveryRecord
	requests  map[string]string
	uncovered map[string]string
}

func newDeliveryJournal(path, epoch string) (*deliveryJournal, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &deliveryJournal{waiting: map[string]string{}, executions: map[string]bool{}, epoch: epoch, file: f, records: map[string]deliveryRecord{}, requests: map[string]string{}, uncovered: map[string]string{}}, nil
}

func (j *deliveryJournal) append(r deliveryRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if j.size+int64(len(b)) > 64<<20 {
		return ports.ErrChatDeliveryUnverified
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

	outbound := e.Method == "session/prompt" || e.Method == "turn/start" || e.Method == "turn/steer" || e.Method == "thread/compact/start"
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
			j.uncovered[string(e.ID)] = e.Method
			j.mu.Unlock()
			return frame, nil
		}
	}
	var meta map[string]json.RawMessage
	_ = json.Unmarshal(e.Params["_meta"], &meta)
	var attempt string
	_ = json.Unmarshal(meta[DeliveryMetaKey], &attempt)
	j.mu.Lock()
	defer j.mu.Unlock()
	if attempt == "" {
		j.uncovered[string(e.ID)] = e.Method
		return frame, nil
	}
	if _, exists := j.records[attempt]; exists {
		return nil, ports.ErrChatDeliveryUnverified
	}
	receipt := domain.ChatDeliveryReceipt{AttemptID: attempt, ProviderRequestID: rpcCorrelation(e.ID), ProviderEpoch: j.epoch, Disposition: domain.ChatDeliveryUnknown, Source: "incomplete", ObservedAt: time.Now().UTC()}
	if receipt.Validate() != nil {
		return nil, ports.ErrChatDeliveryUnverified
	}
	record := deliveryRecord{Receipt: receipt, Method: e.Method}
	if err := j.append(record); err != nil {
		return nil, err
	}
	j.records[attempt], j.requests[string(e.ID)] = record, attempt
	delete(meta, DeliveryMetaKey)
	if len(meta) == 0 {
		delete(e.Params, "_meta")
	} else {
		e.Params["_meta"], _ = json.Marshal(meta)
	}
	var envelope map[string]json.RawMessage
	_ = json.Unmarshal(frame, &envelope)
	envelope["params"], _ = json.Marshal(e.Params)
	b, err := json.Marshal(envelope)
	return append(b, '\n'), err
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
			TurnID string `json:"turnId"`
			Item   struct {
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
		// Unmanaged input/control effects remain an explicit coverage gap.
		// A response alone cannot settle an unjournaled steer or compact.
		attempt, ok := j.requests[string(e.ID)]
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
		if err := j.append(record); err != nil {
			return err
		}
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
		if e.Method == "turn/started" {
			j.executions[native] = true
		}
		if e.Method == "turn/completed" {
			delete(j.executions, native)
		}
		if strings.Contains(e.Method, "requestApproval") && len(e.ID) > 0 {
			j.waiting[string(e.ID)] = native
		}
		if e.Method == "item/started" && (e.Params.Item.Type == "commandExecution" || e.Params.Item.Type == "mcpToolCall") {
			j.waiting["item:"+e.Params.Item.ID] = native
		}
		if e.Method == "item/completed" {
			delete(j.waiting, "item:"+e.Params.Item.ID)
		}
		for attempt, record := range j.records {
			if record.NativeTurn == "" || record.NativeTurn != native || record.Receipt.Disposition != domain.ChatDeliveryAccepted {
				continue
			}
			delete(j.executions, native)
			record.Receipt.Source = "provider_event"
			record.Receipt.ObservedAt = time.Now().UTC()
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
			if e.Method == "turn/started" || e.Method == "turn/completed" || strings.Contains(e.Method, "requestApproval") || e.Method == "item/started" || e.Method == "item/completed" {
				if err := j.append(record); err != nil {
					return err
				}
			}
			j.records[attempt] = record
		}
	}

	return nil
}

func (j *deliveryJournal) query(lookups []ports.ChatDeliveryLookup, flush bool) ([]domain.ChatDeliveryReceipt, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if flush {
		if len(j.uncovered) > 0 || len(j.executions) > 0 {
			return nil, ports.ErrChatShutdownNotSettled
		}
		if err := j.file.Sync(); err != nil {
			return nil, err
		}
	}
	out := make([]domain.ChatDeliveryReceipt, 0, len(lookups))
	for _, lookup := range lookups {
		record, ok := j.records[lookup.AttemptID]
		if !ok || lookup.ProviderEpoch != "" && lookup.ProviderEpoch != j.epoch || lookup.ProviderRequestID != "" && lookup.ProviderRequestID != record.Receipt.ProviderRequestID {
			return nil, ports.ErrChatDeliveryUnverified
		}
		receipt := record.Receipt
		for _, native := range j.waiting {
			if record.NativeTurn != "" && record.NativeTurn == native && !receipt.ExecutionSettled {
				receipt.ObservedAt = time.Now().UTC()
			}
		}
		receipt.CustodyFlushed = flush
		if flush {
			record.Receipt = receipt
			if err := j.append(record); err != nil {
				return nil, err
			}
		}
		out = append(out, receipt)
	}
	return out, nil
}

// DeliveryClient uses the existing authenticated host control endpoint.
type DeliveryClient struct{ descriptor Descriptor }

// QueryDeliveryAttempt reads only the original host correlation.
func (c *DeliveryClient) QueryDeliveryAttempt(ctx context.Context, key ports.ChatDeliveryLookup) (domain.ChatDeliveryReceipt, error) {
	receipts, err := c.call(ctx, []ports.ChatDeliveryLookup{key}, false)
	if err != nil || len(receipts) != 1 {
		return domain.ChatDeliveryReceipt{}, errors.Join(ports.ErrChatDeliveryUnverified, err)
	}
	return receipts[0], nil
}

// FenceAndFlushOutbound confirms journal custody before transport detach.
func (c *DeliveryClient) FenceAndFlushOutbound(ctx context.Context, keys []ports.ChatDeliveryLookup) ([]domain.ChatDeliveryReceipt, error) {
	return c.call(ctx, keys, true)
}
func (c *DeliveryClient) call(ctx context.Context, keys []ports.ChatDeliveryLookup, flush bool) ([]domain.ChatDeliveryReceipt, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.descriptor.Address)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	finish := bindConnToContext(ctx, conn)
	action := "delivery-query"
	if flush {
		action = "delivery-flush"
	}
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
