// Provider processes are controlled by the fixture; conversation HTTP and SSE
// use the real daemon and the production ChatWorkspace/hooks without mocks.
import { useEffect, useSyncExternalStore } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider, useMutation, useQuery } from "@tanstack/react-query";
import { I18nextProvider } from "react-i18next";
import { appI18n } from "../../src/renderer/i18n";
import { ChatWorkspace } from "../../src/renderer/components/chat/ChatWorkspace";
import { Button } from "../../src/renderer/components/ui/button";
import { TooltipProvider } from "../../src/renderer/components/ui/tooltip";
import { useConversation, useConversationCommands } from "../../src/renderer/hooks/useConversation";
import { setApiBaseUrl } from "../../src/renderer/lib/api-client";
import { createEventTransport } from "../../src/renderer/lib/event-transport";
import { getEventsConnectionState, subscribeEventsConnection } from "../../src/renderer/lib/events-connection";
import "../../src/renderer/styles.css";

function loopbackOrigin(raw: string): string {
	const url = new URL(raw);
	if (url.protocol !== "http:" || url.hostname !== "127.0.0.1" || !url.port || url.username || url.password) {
		throw new Error("The connected fixture requires an isolated loopback URL.");
	}
	return url.origin;
}
const fixtureBaseUrl = loopbackOrigin(new URLSearchParams(window.location.search).get("fixtureBaseUrl") ?? "http://127.0.0.1");
const queryClient = new QueryClient();

function ConnectedBackground({ sessionId }: { sessionId: string }) {
	const { snapshot, error } = useConversation(sessionId);
	const commands = useConversationCommands(sessionId);
	const events = useSyncExternalStore(subscribeEventsConnection, getEventsConnectionState);
	useEffect(() => createEventTransport(queryClient).connect(), []);
	const trigger = useMutation({ mutationFn: async (phase: "start" | "finish") => {
		const response = await fetch(`${fixtureBaseUrl}/test/background/${phase}`, { method: "POST" });
		if (!response.ok) throw new Error(`Background fixture rejected ${phase}: ${response.status}`);
	} });
	return <div className="flex h-full flex-col">
		<div className="flex items-center gap-3 border-b border-border bg-sidebar px-3 py-2 text-xs text-muted-foreground">
			<span>Isolated daemon · controlled Claude provider</span>
			<span data-testid="background-events">Events: {events}</span>
			<Button size="sm" onClick={() => trigger.mutate("start")}>Start background task</Button>
			<Button size="sm" onClick={() => trigger.mutate("finish")}>Finish background task</Button>
			{trigger.error ? <p role="alert">{trigger.error.message}</p> : null}
		</div>
		<div className="min-h-0 flex-1">{snapshot
			? <ChatWorkspace snapshot={snapshot} sessionTitle="Harness background turns" retryControl={commands.retryControl} />
			: <p role="status">{error ?? "Loading isolated conversation…"}</p>}</div>
	</div>;
}

function Loader() {
	const fixture = useQuery({ queryKey: ["background-fixture"], retry: false, queryFn: async () => {
		const response = await fetch(`${fixtureBaseUrl}/test/background`);
		if (!response.ok) throw new Error(`Fixture unavailable: ${response.status}`);
		const data: unknown = await response.json();
		if (!data || typeof data !== "object" || !("baseUrl" in data) || typeof data.baseUrl !== "string" || !("sessionId" in data) || typeof data.sessionId !== "string") {
			throw new Error("Invalid isolated fixture metadata");
		}
		setApiBaseUrl(loopbackOrigin(data.baseUrl));
		return { sessionId: data.sessionId };
	} });
	return fixture.data ? <ConnectedBackground sessionId={fixture.data.sessionId} />
		: <p role="status">{fixture.error?.message ?? "Connecting to isolated daemon…"}</p>;
}

const root = document.getElementById("root");
if (!root) throw new Error("Missing fixture root");
createRoot(root).render(<QueryClientProvider client={queryClient}>
	<I18nextProvider i18n={appI18n}><TooltipProvider><Loader /></TooltipProvider></I18nextProvider>
</QueryClientProvider>);
