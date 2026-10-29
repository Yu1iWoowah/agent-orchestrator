import { beforeEach, describe, expect, it } from "vitest";
import {
	createConversationFrameStore,
	conversationFrameSessionKey,
	conversationFrameStorageKey,
} from "./conversation-frame-store";

describe("conversation frame preferences", () => {
	beforeEach(() => localStorage.clear());

	it("restores an explicit open Plan and closed question after reload", () => {
		const key = conversationFrameSessionKey("session-1");
		const first = createConversationFrameStore(localStorage);
		first.getState().setFrameOpen(key, "plan", true);
		first.getState().setFrameOpen(key, "attention", false);
		first.getState().setFrameSize(key, "plan", { width: 520, height: 600 });
		first.getState().swapFrameSlots(key);
		const reloaded =
			createConversationFrameStore(localStorage).getState().sessions[key];
		expect(reloaded.plan).toMatchObject({
			userOpen: true,
			preferredWidth: 520,
			preferredHeight: 600,
			slot: "top",
		});
		expect(reloaded.attention).toMatchObject({
			userOpen: false,
			slot: "bottom",
		});
	});

	it("isolates local, remote and different sessions without delimiter collisions", () => {
		const store = createConversationFrameStore(localStorage);
		const local = conversationFrameSessionKey("session:a");
		const remote = conversationFrameSessionKey("session:a", "local");
		const other = conversationFrameSessionKey("session:b");
		store.getState().setFrameOpen(local, "plan", true);
		expect(local).not.toBe(remote);
		expect(store.getState().sessions[remote]).toBeUndefined();
		expect(store.getState().sessions[other]).toBeUndefined();
	});

	it("ignores invalid saved state rather than mounting overlapping or unbounded frames", () => {
		localStorage.setItem(
			conversationFrameStorageKey,
			JSON.stringify({
				sessions: { invalid: { plan: { preferredWidth: "bad", slot: "top" } } },
			}),
		);
		const store = createConversationFrameStore(localStorage);
		expect(store.getState().sessions.invalid).toBeUndefined();
		const key = conversationFrameSessionKey("fresh");
		store.getState().setFrameOpen(key, "attention", true);
		expect(store.getState().sessions[key].plan).toMatchObject({
			userOpen: false,
			preferredWidth: 360,
			preferredHeight: 480,
		});
		expect(store.getState().sessions[key].attention).toMatchObject({
			userOpen: true,
			preferredWidth: 320,
			preferredHeight: 360,
		});
	});
});

it("persists only document dimensions in the shared frame preference namespace", () => {
	const store = createConversationFrameStore(localStorage);
	store.getState().setDocumentSize("chat", { width: 500, height: 450 });
	store.getState().setFrameOpen("chat", "plan", true);
	expect(
		createConversationFrameStore(localStorage).getState().documentSizes.chat,
	).toEqual({ width: 500, height: 450 });
	expect(localStorage.getItem(conversationFrameStorageKey)).not.toContain(
		"canonicalUri",
	);
});
