package persistenthost

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

func TestACPRelayRetainsBackgroundNotifications(t *testing.T) {
	relay := newTestACPRelay(t)
	relay.state.RetainUnpromptedEvents = true
	frame := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"background answer"}}}}` + "\n")
	got, retained := relayProviderFrame(t, relay, frame, 1, true)
	if !retained || !bytes.Contains(got, []byte(ACPEventIDMetaKey)) {
		t.Fatalf("background output lacks durable replay identity: retained=%v frame=%s", retained, got)
	}
	replay := relayReplayFrames(t, relay)
	if len(replay) != 1 || !bytes.Equal(replay[0], got) {
		t.Fatalf("background replay = %q, want one identical frame %s", replay, got)
	}
}

func TestACPRelayBackgroundAckPreservesNextCycleAndPrompt(t *testing.T) {
	relay := newTestACPRelay(t)
	// Negotiate the generic retention capability through session/new.
	relayClientFrame(t, relay, []byte(`{"id":1,"method":"session/new","params":{"_meta":{"ao.retainUnpromptedEvents":true}}}`+"\n"), 1)
	if !relay.state.RetainUnpromptedEvents {
		t.Fatal("unprompted event retention was not negotiated")
	}
	background := func(text string) []byte {
		got, retained := relayProviderFrame(t, relay, []byte(`{"method":"session/update","params":{"marker":"`+text+`"}}`+"\n"), 1, true)
		if !retained {
			t.Fatal("lost background frame")
		}
		return got
	}
	first := background("first")
	prompt := relayClientFrame(t, relay, []byte(`{"id":2,"method":"session/prompt","params":{}}`+"\n"), 1)
	update, _ := relayProviderFrame(t, relay, []byte(`{"method":"session/update","params":{"marker":"prompt"}}`+"\n"), 1, true)
	relayProviderFrame(t, relay, []byte(`{"id":`+frameID(t, prompt)+`,"result":{"stopReason":"end_turn"}}`+"\n"), 1, true)
	last := background("second")
	frames := relayReplayFrames(t, relay)
	if len(frames) != 4 || !bytes.Equal(frames[0], first) || !bytes.Equal(frames[1], update) || !bytes.Contains(frames[2], []byte(ACPPromptResultMethod)) || !bytes.Equal(frames[3], last) {
		t.Fatalf("replay reordered interleaved turns: %q", frames)
	}
	ack := func(method, id string) {
		frame, _ := json.Marshal(map[string]any{"method": method, "params": map[string]string{"eventId": id}})
		if _, err := relay.clientFrame(context.Background(), frame, 2); err != nil {
			t.Fatal(err)
		}
	}
	ack(ACPUnpromptedAckMethod, frameMetaString(t, first, ACPEventIDMetaKey))
	if frames = relayReplayFrames(t, relay); len(frames) != 3 || !bytes.Equal(frames[0], update) {
		t.Fatalf("background ACK removed prompt or later cycle: %q", frames)
	}
	ack(ACPPromptAckMethod, relay.state.PendingResultEventID)
	if frames = relayReplayFrames(t, relay); len(frames) != 1 || !bytes.Equal(frames[0], last) {
		t.Fatalf("prompt ACK removed later background cycle: %q", frames)
	}
	ack(ACPUnpromptedAckMethod, frameMetaString(t, last, ACPEventIDMetaKey))
	if frames = relayReplayFrames(t, relay); len(frames) != 0 || relay.unpromptedJournal.size != 0 {
		t.Fatalf("committed background journal not released: %q", frames)
	}
}
