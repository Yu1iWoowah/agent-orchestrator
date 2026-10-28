import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { beginModalHandoff } from "./modal-handoff";

const standIn = () => document.querySelector("[data-slot='modal-handoff-backdrop']");
const flushObservers = () => Promise.resolve();

function addOverlay(state?: "open" | "closed"): HTMLElement {
	const overlay = document.createElement("div");
	overlay.className = "dialog-overlay";
	if (state) overlay.dataset.state = state;
	document.body.appendChild(overlay);
	return overlay;
}

beforeEach(() => {
	vi.useFakeTimers();
});

afterEach(async () => {
	try {
		// Complete both the handoff and its delayed fade reset before removing
		// the DOM, so finish() also disconnects every handoff observer.
		await vi.runAllTimersAsync();
		expect(vi.getTimerCount()).toBe(0);
		expect(standIn()).toBeNull();
		expect(document.documentElement.dataset.modalHandoff).toBeUndefined();
	} finally {
		vi.useRealTimers();
		document.body.innerHTML = "";
		delete document.documentElement.dataset.modalHandoff;
	}
});

describe("beginModalHandoff", () => {
	it("leaves no observer that can adopt a backdrop after teardown", ({ onTestFinished }) => {
		onTestFinished(async () => {
			// A leaked observer would adopt this backdrop and queue a fade reset.
			// Keep that post-teardown probe on a controlled clock too.
			vi.useFakeTimers();
			try {
				const later = addOverlay("open");
				await flushObservers();
				expect(later.hasAttribute("data-modal-handoff")).toBe(false);
				expect(vi.getTimerCount()).toBe(0);
			} finally {
				await vi.runAllTimersAsync();
				vi.useRealTimers();
				document.body.innerHTML = "";
				delete document.documentElement.dataset.modalHandoff;
			}
		});
		beginModalHandoff();
		expect(standIn()).not.toBeNull();
	});

	it("holds a stand-in backdrop until the next modal's backdrop arrives, then marks that one", async () => {
		const palette = addOverlay();
		beginModalHandoff();
		expect(standIn()).not.toBeNull();
		expect(document.documentElement.dataset.modalHandoff).toBe("true");

		palette.remove();
		await flushObservers();
		expect(standIn()).not.toBeNull();

		const next = addOverlay("open");
		await flushObservers();
		expect(standIn()).toBeNull();
		expect(next.hasAttribute("data-modal-handoff")).toBe(true);
		await vi.advanceTimersByTimeAsync(50);
		expect(document.documentElement.dataset.modalHandoff).toBeUndefined();
	});

	it("takes a closing backdrop that reopens as the arriving one", async () => {
		const closing = addOverlay("closed");
		beginModalHandoff();
		closing.dataset.state = "open";
		await flushObservers();
		expect(standIn()).toBeNull();
		expect(closing.hasAttribute("data-modal-handoff")).toBe(true);
	});

	it("ignores backdrops already open when it starts", async () => {
		const open = addOverlay("open");
		beginModalHandoff();
		open.dataset.state = "open";
		await flushObservers();
		expect(standIn()).not.toBeNull();
		expect(open.hasAttribute("data-modal-handoff")).toBe(false);
	});

	it("lets go after a short timeout when no modal opens", () => {
		beginModalHandoff();
		vi.advanceTimersByTime(249);
		expect(standIn()).not.toBeNull();
		vi.advanceTimersByTime(1);
		expect(standIn()).toBeNull();
		vi.advanceTimersByTime(50);
		expect(document.documentElement.dataset.modalHandoff).toBeUndefined();
	});
});
