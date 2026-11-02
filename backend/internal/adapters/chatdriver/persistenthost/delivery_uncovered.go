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
	// These methods never submit model input or start a native turn. The RPC
	// response (including a null result or refusal) ends their control request.
	if gap.Method == "config/mcpServer/reload" || gap.Method == "thread/delete" {
		if len(result) > 0 || len(rpcError) > 0 {
			delete(j.unsettled, id)
		}
		return
	}
	if len(rpcError) > 0 {
		var refusal struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		decoded := json.Unmarshal(rpcError, &refusal) == nil
		steerRejected := decoded && gap.Method == "turn/steer" && refusal.Code == -32600 && (refusal.Message == "no active turn to steer" || refusal.Message == "cannot steer a compact turn")
		if steerRejected || gap.Method == "turn/steer" && gap.TerminalObserved || decoded && (refusal.Code == -32601 || refusal.Code == -32600 && strings.Contains(refusal.Message, "unknown variant `"+gap.Method+"`")) {
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
			j.executions[gap.NativeTurn] = gap.ThreadID
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
			if j.uncovered[id] == "" {
				delete(j.unsettled, id)
			} else {
				gap.TerminalObserved = true
				j.unsettled[id] = gap
			}
		}
	}
}

// A read can reconcile only operations whose RPC had already ended when this
// exact includeTurns request was forwarded. It cannot settle later requests.
type nativeDeliveryRead struct {
	ThreadID   string
	Gaps       map[string]string
	Executions []string
}

func (j *deliveryJournal) prepareDeliveryRead(id string, params map[string]json.RawMessage) {
	var thread string
	var include bool
	if json.Unmarshal(params["threadId"], &thread) != nil || thread == "" || json.Unmarshal(params["includeTurns"], &include) != nil || !include {
		return
	}
	check := nativeDeliveryRead{ThreadID: thread, Gaps: map[string]string{}}
	for key, gap := range j.unsettled {
		if gap.ThreadID == thread && j.uncovered[key] == "" {
			check.Gaps[key] = gap.NativeTurn
		}
	}
	for native, executionThread := range j.executions {
		if executionThread == thread {
			check.Executions = append(check.Executions, native)
		}
	}
	if len(check.Gaps) == 0 && len(check.Executions) == 0 {
		return
	}
	if j.readChecks == nil {
		j.readChecks = map[string]nativeDeliveryRead{}
	}
	if len(j.readChecks) >= maxDeliveryRecords {
		// A missing read response must not prevent a new recovery query. Losing an
		// old read correlation is conservative: its eventual reply proves nothing.
		for key := range j.readChecks {
			delete(j.readChecks, key)
			break
		}
	}
	j.readChecks[id] = check
}
func (j *deliveryJournal) observeDeliveryRead(id string, result, rpcError json.RawMessage) {
	check, ok := j.readChecks[id]
	delete(j.readChecks, id)
	if !ok || len(rpcError) > 0 {
		return
	}
	var response struct {
		Thread struct {
			ID     string `json:"id"`
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
			Turns *[]struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if json.Unmarshal(result, &response) != nil || response.Thread.ID != check.ThreadID || response.Thread.Turns == nil {
		return
	}
	terminal := map[string]bool{}
	idle := response.Thread.Status.Type == "idle"
	for _, turn := range *response.Thread.Turns {
		switch turn.Status {
		case "completed", "interrupted", "failed":
			terminal[turn.ID] = true
		case "inProgress":
			idle = false
		default:
			idle = false
		}
	}
	for _, native := range check.Executions {
		if j.executions[native] == check.ThreadID && (idle || terminal[native]) {
			delete(j.executions, native)
		}
	}
	for key, boundTurn := range check.Gaps {
		gap, exists := j.unsettled[key]
		if !exists || gap.ThreadID != check.ThreadID || gap.NativeTurn != boundTurn || j.uncovered[key] != "" {
			continue
		}
		target := gap.NativeTurn
		if target == "" && gap.Method == "turn/steer" {
			target = gap.RequestedTurn
		}
		// An execution-starting request may still be queued before any turn is
		// visible. Only identified turns and known synchronous/steering methods
		// can use idle proof. Other gaps retain the explicit termination exit.
		idleSettles := false
		if idle {
			switch gap.Method {
			case "turn/steer", "thread/rollback", "thread/revert", "thread/fork":
				idleSettles = true
			case "thread/compact/start", "turn/start":
				idleSettles = gap.NativeTurn != ""
			}
		}
		if idleSettles || target != "" && terminal[target] {
			delete(j.unsettled, key)
			if terminal[target] {
				delete(j.executions, target)
			}
		}
	}
}
