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
	revealFragment: boolean;
}
export type DocumentOpening =
	| {
			status: "loading";
			id: number;
			input: DocumentInput;
			originalLink?: string;
			preservePosition: boolean;
	  }
	| {
			status: "error";
			preservePosition: boolean;
			id: number;
			input: DocumentInput;
			originalLink?: string;
			code: string;
			message: string;
	  };
export interface DocumentSession {
	history: DocumentVisit[];
	index: number;
	foreground: boolean;
	opening: DocumentOpening | null;
}
export type DocumentReaderContext = {
	key: string;
	connection: DocumentConnection;
} & ({ scope?: "session"; sessionId: string } | { scope: "gallery"; sessionId?: never });
interface DocumentReadingState {
	sessions: Record<string, DocumentSession>;
	serial: number;
	reader: DocumentReaderContext | null;
	register: (reader: DocumentReaderContext) => void;
	unregister: (key: string) => void;
	open: (
		key: string,
		input: DocumentInput,
		options?: { preservePosition?: boolean; originalLink?: string },
	) => void;
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
	revealFragment = document.target.fragment !== undefined,
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
				index === existing
					? { ...item, document, epoch, revealFragment }
					: item,
			),
		};
	const history = [
		...session.history.slice(0, session.index + 1),
		{
			document,
			position: { scrollTop: 0, zoom: 1 },
			mode: "preview" as const,
			epoch,
			revealFragment,
		},
	].slice(-20);
	return {
		history,
		index: history.length - 1,
		foreground: true,
		opening: null,
	};
}
function stepHistory(
	session: DocumentSession,
	direction: -1 | 1,
): DocumentSession {
	const index = session.history.length
		? Math.max(
				0,
				Math.min(session.history.length - 1, session.index + direction),
			)
		: -1;
	return {
		...session,
		index,
		opening: null,
		history: session.history.map((item, n) =>
			n === index ? { ...item, revealFragment: false } : item,
		),
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
		open: (key, input, options) =>
			set((state) => ({
				serial: state.serial + 1,
				sessions: {
					...state.sessions,
					[key]: {
						...(state.sessions[key] ?? empty()),
						foreground: true,
						opening: {
							status: "loading",
							id: state.serial + 1,
							input,
							originalLink: options?.originalLink,
							preservePosition: !!options?.preservePosition,
						},
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
								[key]: visit(
									state.sessions[key],
									document,
									state.serial + 1,
									!state.sessions[key].opening.preservePosition &&
										document.target.fragment !== undefined,
								),
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
								[key]: stepHistory(state.sessions[key], -1),
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
								[key]: stepHistory(state.sessions[key], 1),
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
