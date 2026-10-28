import {
	createContext,
	useContext,
	useMemo,
	useSyncExternalStore,
	type ReactNode,
} from "react";
import { getApiBaseUrl, subscribeApiBaseUrl } from "./api-client";
import type { DocumentInput } from "./document-read";
import {
	useDocumentReadingStore,
	type DocumentReaderContext,
} from "../stores/document-reading-store";
import { conversationFrameSessionKey } from "../stores/conversation-frame-store";
export const DocumentReadingContext =
	createContext<DocumentReaderContext | null>(null);
export function DocumentReadingProvider({
	sessionId,
	hostId,
	cloud,
	children,
}: {
	sessionId: string;
	hostId?: string;
	cloud?: boolean;
	children: ReactNode;
}) {
	const identity = useSyncExternalStore(subscribeApiBaseUrl, getApiBaseUrl);
	const reader = useMemo<DocumentReaderContext>(
		() => ({
			key: JSON.stringify([
				conversationFrameSessionKey(sessionId, hostId),
				cloud ? "cloud" : identity,
			]),
			sessionId,
			connection: {
				kind:
					hostId && hostId !== "local" ? "remote" : cloud ? "cloud" : "local",
				identity: hostId ?? identity,
			},
		}),
		[sessionId, hostId, cloud, identity],
	);
	return (
		<DocumentReadingContext.Provider value={reader}>
			{children}
		</DocumentReadingContext.Provider>
	);
}
export const useDocumentReader = () => useContext(DocumentReadingContext);
export function openReadingDocument(
	reader: DocumentReaderContext,
	input: DocumentInput,
) {
	useDocumentReadingStore.getState().open(reader.key, input);
}
