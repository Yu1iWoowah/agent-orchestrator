import { create } from "zustand";

export type ProviderInputValue = string | number | boolean | string[];
export interface ProviderInputDraft {
	values: Record<string, ProviderInputValue>;
	activeQuestion: number;
	collapsed: boolean;
}

// Volatile drafts survive source/session navigation, not app shutdown. Only the
// original native form writes them, and a successful resolve removes its draft.
export const useProviderInputDraftStore = create<{
	drafts: Record<string, ProviderInputDraft>;
	save: (key: string, draft: ProviderInputDraft) => void;
	clear: (key: string) => void;
	clearSource: (scope: {
		hostId?: string;
		sessionId: string;
		branchId?: string;
		activityId: string;
	}) => void;
}>((set) => ({
	drafts: {},
	save: (key, draft) =>
		set((state) => ({ drafts: { ...state.drafts, [key]: draft } })),
	clear: (key) =>
		set((state) => {
			const drafts = { ...state.drafts };
			delete drafts[key];
			return { drafts };
		}),
	clearSource: ({ hostId, sessionId, branchId, activityId }) =>
		set((state) => {
			const entries = Object.entries(state.drafts);
			const retained = entries.filter(([key]) => {
				let scope: unknown;
				try {
					scope = JSON.parse(key);
				} catch {
					return true;
				}
				// Production keys are [host, session, conversation, branch, activity, revision].
				return (
					!Array.isArray(scope) ||
					scope.length !== 6 ||
					scope[0] !== (hostId ?? null) ||
					scope[1] !== sessionId ||
					scope[3] !== (branchId ?? null) ||
					scope[4] !== activityId
				);
			});
			return retained.length === entries.length
				? state
				: { drafts: Object.fromEntries(retained) };
		}),
}));
