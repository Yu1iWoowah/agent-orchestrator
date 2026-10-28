import { create } from "zustand";
import {
	documentFileKey,
	type DocumentInput,
	type ResolvedDocument,
} from "../lib/document-targets";
import type { DocumentConnection } from "../lib/document-read";

export interface DocumentPosition {
	scrollTop: number;
	zoom: number;
}
export interface DocumentVisit {
	document: ResolvedDocument;
	position: DocumentPosition;
	mode: "preview" | "source";
	epoch: number;
}
export type DocumentOpening =
	| { status: "loading"; id: number; input: DocumentInput }
	| {
			status: "error";
			id: number;
			input: DocumentInput;
			code: string;
			message: string;
	  };
export interface DocumentSession {
	history: DocumentVisit[];
	index: number;
	foreground: boolean;
	opening: DocumentOpening | null;
}
export interface DocumentReaderContext {
	key: string;
	sessionId: string;
	connection: DocumentConnection;
}
interface DocumentReadingState {
	sessions: Record<string, DocumentSession>;
	serial: number;
	reader: DocumentReaderContext | null;
	register: (reader: DocumentReaderContext) => void;
	unregister: (key: string) => void;
	open: (key: string, input: DocumentInput) => void;
	complete: (key: string, id: number, document: ResolvedDocument) => void;
	fail: (key: string, id: number, code: string, message: string) => void;
	show: (key: string, document: ResolvedDocument) => void;
	close: (key: string) => void;
	reveal: (key: string) => void;
	back: (key: string) => void;
	forward: (key: string) => void;
	savePosition: (key: string, position: DocumentPosition) => void;
	setMode: (key: string, mode: DocumentVisit["mode"]) => void;
}
const empty = (): DocumentSession => ({
	history: [],
	index: -1,
	foreground: false,
	opening: null,
});
function visit(
	session: DocumentSession,
	document: ResolvedDocument,
	epoch: number,
): DocumentSession {
	const key = documentFileKey(document.target);
	const existing = session.history.findIndex(
		(item) => documentFileKey(item.document.target) === key,
	);
	if (existing >= 0)
		return {
			...session,
			foreground: true,
			opening: null,
			index: existing,
			history: session.history.map((item, index) =>
				index === existing ? { ...item, document, epoch } : item,
			),
		};
	const history = [
		...session.history.slice(0, session.index + 1),
		{
			document,
			position: { scrollTop: 0, zoom: 1 },
			mode: "preview" as const,
			epoch,
		},
	].slice(-20);
	return {
		history,
		index: history.length - 1,
		foreground: true,
		opening: null,
	};
}
/** Reading targets and history have no persistence middleware; an app restart starts empty. */
export function createDocumentReadingStore() {
	return create<DocumentReadingState>((set) => ({
		sessions: {},
		serial: 0,
		reader: null,
		register: (reader) => set({ reader }),
		unregister: (key) =>
			set((state) => ({
				reader: state.reader?.key === key ? null : state.reader,
				sessions: state.sessions[key]
					? {
							...state.sessions,
							[key]: {
								...state.sessions[key],
								foreground: false,
								opening: null,
							},
						}
					: state.sessions,
			})),
		open: (key, input) =>
			set((state) => ({
				serial: state.serial + 1,
				sessions: {
					...state.sessions,
					[key]: {
						...(state.sessions[key] ?? empty()),
						foreground: true,
						opening: { status: "loading", id: state.serial + 1, input },
					},
				},
			})),
		complete: (key, id, document) =>
			set((state) =>
				state.sessions[key]?.opening?.status !== "loading" ||
				state.sessions[key]?.opening?.id !== id
					? state
					: {
							serial: state.serial + 1,
							sessions: {
								...state.sessions,
								[key]: visit(state.sessions[key], document, state.serial + 1),
							},
						},
			),
		fail: (key, id, code, message) =>
			set((state) =>
				state.sessions[key]?.opening?.status !== "loading" ||
				state.sessions[key]?.opening?.id !== id
					? state
					: {
							sessions: {
								...state.sessions,
								[key]: {
									...state.sessions[key],
									opening: {
										...state.sessions[key].opening,
										status: "error",
										id,
										input: state.sessions[key].opening.input,
										code,
										message,
									},
								},
							},
						},
			),
		show: (key, document) =>
			set((state) => ({
				serial: state.serial + 1,
				sessions: {
					...state.sessions,
					[key]: visit(
						state.sessions[key] ?? empty(),
						document,
						state.serial + 1,
					),
				},
			})),
		close: (key) =>
			set((state) => ({
				sessions: {
					...state.sessions,
					[key]: {
						...(state.sessions[key] ?? empty()),
						foreground: false,
						opening: null,
					},
				},
			})),
		reveal: (key) =>
			set((state) => ({
				sessions: {
					...state.sessions,
					[key]: { ...(state.sessions[key] ?? empty()), foreground: true },
				},
			})),
		back: (key) =>
			set((state) =>
				!state.sessions[key]
					? state
					: {
							sessions: {
								...state.sessions,
								[key]: {
									...state.sessions[key],
									index: Math.max(
										state.sessions[key].history.length ? 0 : -1,
										state.sessions[key].index - 1,
									),
									opening: null,
								},
							},
						},
			),
		forward: (key) =>
			set((state) =>
				!state.sessions[key]
					? state
					: {
							sessions: {
								...state.sessions,
								[key]: {
									...state.sessions[key],
									index: Math.min(
										state.sessions[key].history.length - 1,
										state.sessions[key].index + 1,
									),
									opening: null,
								},
							},
						},
			),
		savePosition: (key, position) =>
			set((state) =>
				!state.sessions[key]
					? state
					: {
							sessions: {
								...state.sessions,
								[key]: {
									...state.sessions[key],
									history: state.sessions[key].history.map((item, index) =>
										index === state.sessions[key].index
											? { ...item, position }
											: item,
									),
								},
							},
						},
			),
		setMode: (key, mode) =>
			set((state) =>
				!state.sessions[key]
					? state
					: {
							sessions: {
								...state.sessions,
								[key]: {
									...state.sessions[key],
									history: state.sessions[key].history.map((item, index) =>
										index === state.sessions[key].index
											? { ...item, mode }
											: item,
									),
								},
							},
						},
			),
	}));
}
export const useDocumentReadingStore = createDocumentReadingStore();
