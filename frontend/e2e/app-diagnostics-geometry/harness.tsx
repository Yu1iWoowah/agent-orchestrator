import { useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { createAppDiagnosticsCollector } from "../../src/renderer/lib/app-diagnostics";
import { useDiagnosticCommit } from "../../src/renderer/lib/app-diagnostics-registry";
import { componentObservation } from "../../src/renderer/lib/app-diagnostics-projectors";
import type { Collector } from "../../src/shared/app-diagnostics";
const queryClient = new QueryClient();
const scenario = new URLSearchParams(location.search).get("case");
const worker = { id: "next.js-1", kind: "worker", status: "working", title: "CANARY_U11_PRIVATE", activity: { state: "active" } };
const project = { id: "next.js", path: "/CANARY_U11_PRIVATE", sessions: [worker] };
queryClient.setQueryData(["workspaces"], [project]);
const collector = createAppDiagnosticsCollector({
	queryClient,
	readRoute: () => ({ routeId: "board", projectId: "next.js", sessionId: "", source: "local", hostId: "", revision: 1 }),
});
declare global {
	interface Window {
		__u11?: { capture: () => Collector; stable: () => boolean };
	}
}
function Fixture() {
	const root = useRef<HTMLDivElement>(null);
	const list = useRef<HTMLDivElement>(null);
	const [ready, setReady] = useState(false);
	useQuery({ queryKey: ["workspaces"], queryFn: async () => [project], staleTime: Infinity });
	const inputs = scenario === "cache-props" ? [] : [worker];
	useDiagnosticCommit(
		"project",
		() => componentObservation("project", project, { expanded: true, live: inputs, workers: inputs, ordered: inputs, listed: inputs }),
		() => root.current,
		() => list.current,
	);
	useEffect(() => {
		collector.enable();
		const before = {
			data: queryClient.getQueryData(["workspaces"]),
			hash: location.hash,
			storage: JSON.stringify(localStorage),
			selection: getSelection()?.toString(),
		};
		window.__u11 = {
			capture: () => collector.capture("next.js"),
			stable: () =>
				queryClient.getQueryData(["workspaces"]) === before.data &&
				location.hash === before.hash &&
				JSON.stringify(localStorage) === before.storage &&
				getSelection()?.toString() === before.selection,
		};
		const timer = setTimeout(() => setReady(true), 50);
		return () => {
			clearTimeout(timer);
			delete window.__u11;
			collector.disable();
		};
	}, []);
	return (
		<>
			<h1>U11 fixture</h1>
			<div ref={root} style={{ width: 300 }}>
				<div ref={list} style={scenario === "zero-height" ? { height: 0, overflow: "hidden" } : undefined}>
					{scenario !== "cache-props" && scenario !== "props-dom" && (
						<div data-session-row="" style={{ height: 32 }}>
							CANARY_U11_PRIVATE
						</div>
					)}
				</div>
			</div>
			<span data-testid="ready">{ready ? "ready" : "loading"}</span>
		</>
	);
}
createRoot(document.getElementById("root")!).render(
	<QueryClientProvider client={queryClient}>
		<Fixture />
	</QueryClientProvider>,
);
