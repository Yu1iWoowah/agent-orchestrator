import { createContext, useContext, useMemo, useSyncExternalStore, type ReactNode } from "react";
import { getApiBaseUrl, subscribeApiBaseUrl } from "./api-client";
import type { DocumentInput } from "./document-read";
import { useDocumentReadingStore, type DocumentReaderContext } from "../stores/document-reading-store";
import { conversationFrameSessionKey } from "../stores/conversation-frame-store";
export const DocumentReadingContext = createContext<DocumentReaderContext | null>(null);
export function DocumentReadingProvider(
	props: { children: ReactNode } & (
		| { scope?: "session"; sessionId: string; hostId?: string; cloud?: boolean }
		| { scope: "gallery"; sessionId?: never; hostId?: never; cloud?: never }
	),
) {
	const { hostId, cloud, children } = props;
	const identity = useSyncExternalStore(subscribeApiBaseUrl, getApiBaseUrl);
	const reader = useMemo<DocumentReaderContext>(() => {
		const connection: DocumentReaderContext["connection"] = {
			kind: hostId && hostId !== "local" ? "remote" : cloud ? "cloud" : "local",
			identity: hostId ?? identity,
		};
		if (props.scope === "gallery") return { scope: "gallery", key: JSON.stringify(["gallery", identity]), connection };
		return {
			scope: "session",
			sessionId: props.sessionId,
			key: JSON.stringify([conversationFrameSessionKey(props.sessionId, hostId), cloud ? "cloud" : identity]),
			connection,
		};
	}, [props.scope, props.sessionId, hostId, cloud, identity]);
	return <DocumentReadingContext.Provider value={reader}>{children}</DocumentReadingContext.Provider>;
}
export const useDocumentReader = () => useContext(DocumentReadingContext);
export function openReadingDocument(reader: DocumentReaderContext, input: DocumentInput, originalLink?: string) {
	useDocumentReadingStore.getState().open(reader.key, input, { originalLink });
}
