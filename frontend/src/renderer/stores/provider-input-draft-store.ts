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
}));
