import { useCallback, useEffect, useRef, useState } from "react";
import { compositionClient } from "../lib/api-client";

export type SessionComposition = Awaited<ReturnType<typeof compositionClient.get>>;
type View = { status: "loading" } | { status: "error" } | { status: "ready"; data: SessionComposition };
export type CompositionAction = "refresh" | "analyze" | "on" | "off" | "inherit" | "purge" | "stop" | "resume";

export function useSessionComposition(sessionId: string) {
	const [state, setState] = useState<{ sessionId: string; view: View }>({ sessionId, view: { status: "loading" } });
	const [pending, setPending] = useState(false);
	const epoch = useRef(0);
	const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
	const load = useCallback(
		async function load(version: number, analyze = false) {
			try {
				const data = await compositionClient.get({ sessionId, ...(analyze ? { query: { analyze: true } } : {}) });
				if (epoch.current !== version) return;
				if (data.sessionId !== sessionId || data.projectId !== "agent-orchestrator") throw new Error("RCA unavailable");
				setState({ sessionId, view: { status: "ready", data } });
				setPending(false);
				timer.current = setTimeout(
					() => {
						void load(version);
					},
					data.status === "running" ? 500 : 5_000,
				);
			} catch {
				if (epoch.current !== version) return;
				setState({ sessionId, view: { status: "error" } });
				setPending(false);
			}
		},
		[sessionId],
	);
	const run = useCallback(
		async (action: CompositionAction) => {
			clearTimeout(timer.current);
			const version = ++epoch.current;
			// Withdraw the old projection before control I/O; an ABA response must
			// never restore numbers from the policy revision being withdrawn.
			setState({ sessionId, view: { status: "loading" } });
			setPending(true);
			try {
				switch (action) {
					case "on":
					case "off":
						await compositionClient.set({ sessionId, enabled: action === "on" });
						break;
					case "inherit":
						await compositionClient.clear({ sessionId });
						break;
					case "purge":
						await compositionClient.purge({ sessionId });
						break;
					case "stop":
					case "resume":
						await compositionClient.emergencyStop({ enabled: action === "stop" });
						break;
					case "refresh":
					case "analyze":
						break;
				}
				if (epoch.current === version) await load(version, action === "analyze");
			} catch {
				if (epoch.current !== version) return;
				setState({ sessionId, view: { status: "error" } });
				setPending(false);
			}
		},
		[load, sessionId],
	);
	useEffect(() => {
		void run("refresh");
		return () => {
			++epoch.current;
			clearTimeout(timer.current);
		};
	}, [run]);
	return { view: state.sessionId === sessionId ? state.view : ({ status: "loading" } satisfies View), pending, run };
}
