package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
	"unicode/utf8"
)

// ReplacementIntakeTransfer records queue relocation/ownership, not native
// acceptance. Detailed original positions remain in the transfer ledger.
type ReplacementIntakeTransfer struct {
	RetargetedMessages    int64     `json:"retargetedMessages"`
	MovedMessages         int64     `json:"movedMessages"`
	SourceConversationIDs []string  `json:"sourceConversationIds"`
	RecordedAt            time.Time `json:"recordedAt"`
}

// ReplacementIntakePayloadHash binds the exact original pending content while
// allowing its canonical conversation/dispatch owner to move. Keep this v1
// frame stable: historical excerpt permissions rely on its exact digest.
func ReplacementIntakePayloadHash(input QueuedTurn) (string, error) {
	for _, value := range []string{input.TurnID, input.Text, input.ClientMessageID, string(input.Origin), input.DeliveryContentJSON, input.SenderSessionID, input.SenderProjectID, input.SenderDisplayName} {
		if !utf8.ValidString(value) {
			return "", ErrReplacementStateInvalid
		}
	}
	frame := struct {
		Version             int
		TurnID              string
		Text                string
		ClientMessageID     string
		Origin              string
		DeliveryContentJSON string
		SenderSessionID     string
		SenderProjectID     string
		SenderDisplayName   string
	}{1, input.TurnID, input.Text, input.ClientMessageID, string(input.Origin), input.DeliveryContentJSON, input.SenderSessionID, input.SenderProjectID, input.SenderDisplayName}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
