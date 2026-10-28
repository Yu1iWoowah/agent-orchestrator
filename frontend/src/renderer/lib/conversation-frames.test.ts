import { describe, expect, it } from "vitest";
import { conversationFrameSafeRect, deriveConversationFrameLayout, defaultConversationFramePreferences } from "./conversation-frames";

describe("conversation frame geometry", () => {
	it("reserves the entire composer dock rather than covering the queue above the textarea", () => {
		expect(conversationFrameSafeRect({
			surface: { left: 100, top: 40, width: 1000, height: 800 },
			viewport: { left: 100, top: 96, width: 1000, height: 600 },
			composer: { left: 216, top: 580, width: 768, height: 260 },
		})).toEqual({ left: 16, top: 72, width: 968, height: 452 });
	});

	it("starts with an expanded question and an independent small Plan entry", () => {
		const preferences = defaultConversationFramePreferences();
		const layout = deriveConversationFrameLayout({ safeRect: { left: 16, top: 16, width: 800, height: 700 }, preferences, availableFrames: ["attention", "plan"] });
		expect(layout.compact).toBe(false);
		expect(layout.frames.attention?.open).toBe(true);
		expect(layout.frames.attention?.rect).toEqual({ left: 496, top: 16, width: 320, height: 360 });
		expect(layout.frames.plan?.open).toBe(false);
		expect(layout.frames.plan?.rect).toEqual({ left: 688, top: 388, width: 128, height: 32 });
	});

	it("keeps both frames open over a narrow conversation even without gutters", () => {
		const preferences = defaultConversationFramePreferences();
		preferences.plan = { ...preferences.plan, userOpen: true };
		const layout = deriveConversationFrameLayout({ safeRect: { left: 16, top: 16, width: 640, height: 700 }, preferences, availableFrames: ["attention", "plan"] });
		expect(layout.compact).toBe(false);
		expect(layout.frames.plan?.open).toBe(true);
		expect(layout.frames.attention?.open).toBe(true);
		const plan = layout.frames.plan?.rect;
		const attention = layout.frames.attention?.rect;
		if (!plan || !attention) throw new Error("Both frames must be placed");
		expect(plan.left + plan.width).toBe(656);
		expect(attention.left + attention.width).toBe(656);
		expect(attention.top + attention.height + 12).toBeLessThanOrEqual(plan.top);
		expect(plan.top + plan.height).toBeLessThanOrEqual(716);
		expect(preferences.plan.preferredHeight).toBe(480);
		expect(preferences.attention.preferredHeight).toBe(360);
	});

	it.each([
		{ kinds: ["attention", "plan"] as const, planOpen: false, width: 260, height: 204 },
		{ kinds: ["attention", "plan"] as const, planOpen: true, width: 280, height: 372 },
	])("uses only the minimum rectangle of the intended open combination ($width x $height)", ({ kinds, planOpen, width, height }) => {
		const preferences = defaultConversationFramePreferences();
		preferences.plan = { ...preferences.plan, userOpen: planOpen };
		const atBoundary = deriveConversationFrameLayout({ safeRect: { left: 0, top: 0, width, height }, preferences, availableFrames: kinds });
		const tooShort = deriveConversationFrameLayout({ safeRect: { left: 0, top: 0, width, height: height - 1 }, preferences, availableFrames: kinds });
		expect(atBoundary.compact).toBe(false);
		expect(tooShort.compact).toBe(true);
		expect(tooShort.frames.attention?.open).toBe(false);
		expect(tooShort.frames.plan?.open).toBe(false);
		expect(preferences.attention.userOpen).toBe(true);
		expect(preferences.plan.userOpen).toBe(planOpen);
	});

	it("does not reserve a fake question frame before the attention feature is connected", () => {
		const layout = deriveConversationFrameLayout({ safeRect: { left: 0, top: 0, width: 320, height: 76 }, preferences: defaultConversationFramePreferences(), availableFrames: ["plan"] });
		expect(layout.frames.attention).toBeUndefined();
		expect(layout.frames.plan?.rect).toEqual({ left: 192, top: 0, width: 128, height: 32 });
	});
	it("handles a Plan-only open combination at 244px and falls back to header below 76px", () => {
		const preferences = defaultConversationFramePreferences();
		preferences.plan.userOpen = true; preferences.attention.userOpen = false;
		const derive = (height: number) => deriveConversationFrameLayout({ safeRect: { left: 0, top: 0, width: 280, height }, preferences, availableFrames: ["plan", "attention"] });
		expect(derive(244).compact).toBe(false);
		expect(derive(243).compact).toBe(true);
		expect(derive(76).headerOnly).toBe(false);
		expect(derive(75).headerOnly).toBe(true);
	});

});
