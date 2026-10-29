import { create } from "zustand";
import {
	defaultConversationFramePreferences,
	frameLimits,
	type ConversationFrameKind,
	type ConversationFramePreference,
	type ConversationFramePreferences,
} from "../lib/conversation-frames";

export const conversationFrameStorageKey = "ao.chat.frames.v1";
export const conversationFrameSessionKey = (sessionId: string, hostId?: string) =>
	JSON.stringify(hostId ? ["remote", hostId, sessionId] : ["local", sessionId]);
const isRecord = (value: unknown): value is Record<string, unknown> =>
	typeof value === "object" && value !== null && !Array.isArray(value);
function validPreference(
	value: unknown,
	kind: ConversationFrameKind,
): value is ConversationFramePreference {
	if (!isRecord(value)) return false;
	const limits = frameLimits[kind];
	return (
		value.side === "right" &&
		(value.slot === "top" || value.slot === "bottom") &&
		typeof value.userOpen === "boolean" &&
		typeof value.preferredWidth === "number" &&
		Number.isFinite(value.preferredWidth) &&
		value.preferredWidth >= limits.minWidth &&
		value.preferredWidth <= limits.maxWidth &&
		typeof value.preferredHeight === "number" &&
		Number.isFinite(value.preferredHeight) &&
		value.preferredHeight >= limits.minHeight &&
		value.preferredHeight <= limits.maxHeight
	);
}
function readPreferences(storage: Storage): Record<string, ConversationFramePreferences> {
	try {
		const saved: unknown = JSON.parse(storage.getItem(conversationFrameStorageKey) ?? "null");
		if (!isRecord(saved) || !isRecord(saved.sessions)) return {};
		const sessions: Record<string, ConversationFramePreferences> = {};
		for (const [key, value] of Object.entries(saved.sessions)) {
			if (
				isRecord(value) &&
				validPreference(value.plan, "plan") &&
				validPreference(value.attention, "attention") &&
				value.plan.slot !== value.attention.slot
			)
				sessions[key] = { plan: value.plan, attention: value.attention };
		}
		return sessions;
	} catch {
		return {};
	}
}
function readDocumentSizes(
	storage: Storage,
): Record<string, { width: number; height: number }> {
	try {
		const saved: unknown = JSON.parse(
			storage.getItem(conversationFrameStorageKey) ?? "null",
		);
		if (!isRecord(saved) || !isRecord(saved.documentSizes)) return {};
		return Object.fromEntries(
			Object.entries(saved.documentSizes).filter(
				(entry): entry is [string, { width: number; height: number }] => {
					const value = entry[1];
					return (
						isRecord(value) &&
						typeof value.width === "number" &&
						Number.isFinite(value.width) &&
						value.width >= 280 &&
						value.width <= 720 &&
						typeof value.height === "number" &&
						Number.isFinite(value.height) &&
						value.height >= 160 &&
						value.height <= 900
					);
				},
			),
		);
	} catch {
		return {};
	}
}

interface ConversationFrameState {
	documentSizes: Record<string, { width: number; height: number }>;
	setDocumentSize: (key: string, size: { width?: number; height?: number }) => void;
	sessions: Record<string, ConversationFramePreferences>;
	setFrameOpen: (key: string, kind: ConversationFrameKind, open: boolean) => void;
	setFrameSize: (
		key: string,
		kind: ConversationFrameKind,
		size: { width?: number; height?: number },
	) => void;
	swapFrameSlots: (key: string) => void;
}
export function createConversationFrameStore(storage: Storage) {
	return create<ConversationFrameState>((set) => {
		const update = (
			key: string,
			change: (current: ConversationFramePreferences) => ConversationFramePreferences,
		) =>
			set((state) => {
				const sessions = {
					...state.sessions,
					[key]: change(state.sessions[key] ?? defaultConversationFramePreferences()),
				};
				try {
					storage.setItem(conversationFrameStorageKey, JSON.stringify({ sessions, documentSizes: state.documentSizes }));
				} catch {
					/* Keep preferences usable when storage is unavailable. */
				}
				return { sessions };
			});
		return {
			sessions: readPreferences(storage),
			documentSizes: readDocumentSizes(storage),
			setDocumentSize: (key, size) =>
				set((state) => {
					const previous = state.documentSizes[key] ?? {
						width: 520,
						height: 600,
					};
					const documentSizes = {
						...state.documentSizes,
						[key]: {
							width: Number.isFinite(size.width)
								? Math.min(720, Math.max(280, size.width!))
								: previous.width,
							height: Number.isFinite(size.height)
								? Math.min(900, Math.max(160, size.height!))
								: previous.height,
						},
					};
					try {
						storage.setItem(
							conversationFrameStorageKey,
							JSON.stringify({ sessions: state.sessions, documentSizes }),
						);
					} catch {
						/* in-memory fallback */
					}
					return { documentSizes };
				}),
			setFrameOpen: (key, kind, userOpen) =>
				update(key, (current) => ({ ...current, [kind]: { ...current[kind], userOpen } })),
			setFrameSize: (key, kind, size) =>
				update(key, (current) => {
					const limits = frameLimits[kind];
					return {
						...current,
						[kind]: {
							...current[kind],
							preferredWidth:
								size.width !== undefined && Number.isFinite(size.width)
									? Math.max(limits.minWidth, Math.min(limits.maxWidth, size.width))
									: current[kind].preferredWidth,
							preferredHeight:
								size.height !== undefined && Number.isFinite(size.height)
									? Math.max(limits.minHeight, Math.min(limits.maxHeight, size.height))
									: current[kind].preferredHeight,
						},
					};
				}),
			swapFrameSlots: (key) =>
				update(key, (current) => ({
					plan: { ...current.plan, slot: current.attention.slot },
					attention: { ...current.attention, slot: current.plan.slot },
				})),
		};
	});
}
export const useConversationFrameStore = createConversationFrameStore(window.localStorage);
