export type ConversationFrameKind = "plan" | "attention";
export interface ConversationFramePreference {
	side: "right";
	slot: "top" | "bottom";
	preferredWidth: number;
	preferredHeight: number;
	userOpen: boolean;
}
export type ConversationFramePreferences = Record<ConversationFrameKind, ConversationFramePreference>;
export interface FrameRect { left: number; top: number; width: number; height: number }
export const frameLimits = {
	plan: { width: 360, height: 480, minWidth: 280, minHeight: 200, maxWidth: 560, maxHeight: 800, entryWidth: 128 },
	attention: { width: 320, height: 360, minWidth: 260, minHeight: 160, maxWidth: 480, maxHeight: 720, entryWidth: 160 },
};
export const frameGap = 12;
export function defaultConversationFramePreferences(): ConversationFramePreferences {
	return {
		plan: { side: "right", slot: "bottom", preferredWidth: 360, preferredHeight: 480, userOpen: false },
		attention: { side: "right", slot: "top", preferredWidth: 320, preferredHeight: 360, userOpen: true },
	};
}
export function conversationFrameSafeRect({ surface, viewport, composer }: { surface: FrameRect; viewport: FrameRect; composer: FrameRect }): FrameRect {
	const left = Math.max(surface.left, viewport.left) + 16;
	const top = Math.max(surface.top, viewport.top) + 16;
	const right = Math.min(surface.left + surface.width, viewport.left + viewport.width) - 16;
	const bottom = Math.min(surface.top + surface.height, viewport.top + viewport.height, composer.top) - 16;
	return { left: left - surface.left, top: top - surface.top, width: Math.max(0, right - left), height: Math.max(0, bottom - top) };
}
export interface ConversationFrameLayout {
	compact: boolean;
	headerOnly: boolean;
	frames: Partial<Record<ConversationFrameKind, { open: boolean; rect: FrameRect }>>;
}
export function deriveConversationFrameLayout({ safeRect: r, preferences, availableFrames }: { safeRect: FrameRect; preferences: ConversationFramePreferences; availableFrames: readonly ConversationFrameKind[] }): ConversationFrameLayout {
	const kinds = [...availableFrames].sort((a, b) => Number(preferences[a].slot === "bottom") - Number(preferences[b].slot === "bottom"));
	const minimumHeight = (kind: ConversationFrameKind) => preferences[kind].userOpen ? frameLimits[kind].minHeight : 32;
	const gapHeight = Math.max(0, kinds.length - 1) * frameGap;
	const compact = kinds.some((kind) => r.width < (preferences[kind].userOpen ? frameLimits[kind].minWidth : frameLimits[kind].entryWidth)) || kinds.reduce((sum, kind) => sum + minimumHeight(kind), gapHeight) > r.height;
	const headerOnly = r.height < kinds.length * 32 + gapHeight || kinds.some((kind) => r.width < frameLimits[kind].entryWidth);
	const frames: ConversationFrameLayout["frames"] = {};
	let top = r.top;
	let remaining = r.height - gapHeight;
	for (const [index, kind] of kinds.entries()) {
		const pref = preferences[kind];
		const limits = frameLimits[kind];
		const open = pref.userOpen && !compact;
		const reserve = kinds.slice(index + 1).reduce((sum, next) => sum + (compact ? 32 : minimumHeight(next)), 0);
		const width = Math.min(r.width, open ? Math.max(limits.minWidth, Math.min(pref.preferredWidth, limits.maxWidth)) : limits.entryWidth);
		const height = open ? Math.min(Math.max(limits.minHeight, Math.min(pref.preferredHeight, limits.maxHeight)), remaining - reserve) : 32;
		frames[kind] = { open, rect: { left: r.left + r.width - width, top, width, height } };
		top += height + frameGap;
		remaining -= height;
	}
	return { compact, headerOnly, frames };
}
