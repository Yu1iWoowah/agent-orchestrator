package persistenthost

import (
	"encoding/json"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// A response ends RPC custody, not necessarily the execution it initiated.
// Missing native acceptance/terminal proof stays a coverage gap for PR3's
// producer ledger; it cannot vanish from an empty graceful-shutdown flush.
type nativeDeliveryGap struct {
	Method, ThreadID, NativeTurn, RequestedTurn string
	Acknowledged                                bool
	TerminalObserved                            bool
}

func (j *deliveryJournal) trackUncovered(id, method string, params map[string]json.RawMessage) {
	gap := nativeDeliveryGap{Method: method}
	_ = json.Unmarshal(params["threadId"], &gap.ThreadID)
	if method == "turn/steer" {
		_ = json.Unmarshal(params["expectedTurnId"], &gap.RequestedTurn)
	}
	j.uncovered[id] = method
	j.unsettled[id] = gap
}

func (j *deliveryJournal) observeUncoveredResponse(id string, result, rpcError json.RawMessage) {
	gap, ok := j.unsettled[id]
	delete(j.uncovered, id)
	if !ok {
		return
	}
	if len(rpcError) > 0 {
		var refusal struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(rpcError, &refusal) == nil && (refusal.Code == -32601 || refusal.Code == -32600 && strings.Contains(refusal.Message, "unknown variant `"+gap.Method+"`")) {
			// Native method-not-found explicitly proves the handler never ran;
			// ordinary RPC/transport/validation errors do not provide that proof.
			delete(j.unsettled, id)
		}
		return
	}
	if len(result) == 0 || string(result) == "null" {
		return
	}
	gap.Acknowledged = true
	j.unsettled[id] = gap
	switch gap.Method {
	case "session/prompt":
		var response struct {
			StopReason string `json:"stopReason"`
		}
		if json.Unmarshal(result, &response) == nil && response.StopReason != "" {
			delete(j.unsettled, id)
		}
	case "turn/start":
		var response struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(result, &response) == nil && response.Turn.ID != "" {
			gap.NativeTurn = response.Turn.ID
			j.unsettled[id] = gap
			j.executions[gap.NativeTurn] = true
		}
	case "thread/rollback", "thread/revert", "thread/fork":
		// These control methods return the completed synchronous operation.
		delete(j.unsettled, id)
	case "turn/steer":
		var response struct {
			TurnID string `json:"turnId"`
		}
		if json.Unmarshal(result, &response) != nil || response.TurnID == "" || response.TurnID != gap.RequestedTurn {
			return
		}
		gap.NativeTurn = response.TurnID
		j.unsettled[id] = gap
		if gap.TerminalObserved {
			delete(j.unsettled, id)
			return
		}
		// Steering carries content into an existing turn. An exact managed L2
		// record will participate in the checkpoint; otherwise wait for its terminal.
		if gap.NativeTurn != "" {
			for _, record := range j.records {
				if record.NativeTurn == gap.NativeTurn && record.NativeThread == gap.ThreadID && record.Receipt.Disposition == domain.ChatDeliveryAccepted && record.Receipt.Complete {
					delete(j.unsettled, id)
					break
				}
			}
		}
	}
}

func (j *deliveryJournal) observeUncoveredEvent(method, thread, native, itemType string) {
	// Compact ACK has no native ID. Only an unambiguous compaction item in its
	// original thread binds the later execution; unrelated turn/started cannot.
	if ((method == "item/started" || method == "item/completed") && itemType == "contextCompaction" || method == "thread/compacted") && thread != "" {
		candidate := ""
		count := 0
		for id, gap := range j.unsettled {
			if gap.Method == "thread/compact/start" && gap.ThreadID == thread && (gap.Acknowledged || j.uncovered[id] != "") && gap.NativeTurn == "" {
				candidate = id
				count++
			}
		}
		if count == 1 {
			gap := j.unsettled[candidate]
			gap.NativeTurn = native
			j.unsettled[candidate] = gap
		}
	}
	if method != "turn/completed" {
		return
	}
	for id, gap := range j.unsettled {
		if gap.NativeTurn != "" && gap.NativeTurn == native && (gap.ThreadID == "" || gap.ThreadID == thread) {
			delete(j.unsettled, id)
		} else if gap.Method == "turn/steer" && gap.RequestedTurn == native && gap.ThreadID == thread {
			gap.TerminalObserved = true
			j.unsettled[id] = gap
		}
	}
}
