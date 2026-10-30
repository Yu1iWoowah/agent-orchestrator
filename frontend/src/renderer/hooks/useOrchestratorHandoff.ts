import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { apiErrorMessage } from "../lib/api-client";
import { clientForSessionHost } from "../lib/host-clients";
import type { components } from "../../api/schema";

export interface HandoffSource {
	projectId: string;
	sourceSessionId: string;
	hostId?: string;
}

export type HandoffReceipt = Pick<components["schemas"]["OrchestratorReplacement"], "id" | "phase" | "readCommand">;

interface StoredIntent {
	operationKey: string;
	receipt?: HandoffReceipt;
}

class HandoffStorageError extends Error {}

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
function validID(value: unknown): value is string {
	return typeof value === "string" && uuid.test(value) && value !== "00000000-0000-0000-0000-000000000000";
}

function storageKey(source: HandoffSource): string {
	return `ao.handoff.intent.v1:${[source.hostId ?? "local", source.projectId, source.sourceSessionId].map(encodeURIComponent).join(":")}`;
}

function receipt(value: unknown): HandoffReceipt | undefined {
	if (!value || typeof value !== "object" || !("id" in value) || !validID(value.id) || !("phase" in value) || typeof value.phase !== "string") return undefined;
	return { id: value.id, phase: value.phase, readCommand: `ao orchestrator replacement get ${value.id} --json` };
}

function readIntent(storage: Storage, key: string): StoredIntent | undefined {
	let raw: string | null;
	try { raw = storage.getItem(key); } catch { throw new HandoffStorageError(); }
	if (!raw || raw.length > 4096) return undefined;
	let value: unknown;
	try { value = JSON.parse(raw); } catch { return undefined; }
	if (!value || typeof value !== "object" || !("operationKey" in value) || !validID(value.operationKey)) return undefined;
	return { operationKey: value.operationKey, receipt: "receipt" in value ? receipt(value.receipt) : undefined };
}

// Persist identity before HTTP. Reopening the view after an unknown response
// replays the same operation; a repeated accepted request is only a lookup.
export async function acceptOrchestratorHandoff(source: HandoffSource, storage: Storage): Promise<HandoffReceipt> {
	const key = storageKey(source);
	const prior = readIntent(storage, key);
	const intent = prior && prior.receipt?.phase !== "cancelled" && prior.receipt?.phase !== "rejected"
		? prior : { operationKey: crypto.randomUUID() };
	if (intent !== prior) {
		try { storage.setItem(key, JSON.stringify(intent)); } catch { throw new HandoffStorageError(); }
	}
	const { data, error, response } = await clientForSessionHost(source.hostId).POST("/api/v1/projects/{id}/orchestrator/replacements", {
		params: { path: { id: source.projectId } },
		body: { sourceSessionId: source.sourceSessionId, operationKey: intent.operationKey },
	});
	if (error || !data) {
		if (response.status >= 400 && response.status < 500) {
			try { storage.removeItem(key); } catch { /* Keep the prior identity if storage becomes unavailable. */ }
		}
		throw new Error(apiErrorMessage(error));
	}
	const accepted = receipt(data.operation);
	if (!accepted || data.operation.operationKey !== intent.operationKey || data.operation.projectId !== source.projectId || data.operation.sourceSessionId !== source.sourceSessionId) throw new Error("ORCHESTRATOR_HANDOFF_RECEIPT_INVALID");
	// Acceptance is known even if this update fails: the original persisted key
	// remains available for safe replay after a restart.
	try { storage.setItem(key, JSON.stringify({ ...intent, receipt: accepted })); } catch { /* The request key was already saved before HTTP. */ }
	return accepted;
}

export function useOrchestratorHandoff(source: HandoffSource) {
	const { t } = useTranslation();
	const key = storageKey(source);
	const active = useRef(key);
	active.current = key;
	useEffect(() => {
		active.current = key;
		return () => { if (active.current === key) active.current = ""; };
	}, [key]);
	const initial = useMemo(() => {
		try { return readIntent(window.localStorage, key)?.receipt; } catch { return undefined; }
	}, [key]);
	const [last, setLast] = useState<{ key: string; value: HandoffReceipt }>();
	const flight = useRef<{ key: string; promise: Promise<HandoffReceipt> } | undefined>(undefined);
	const { projectId, sourceSessionId, hostId } = source;
	const request = useCallback(() => {
		if (flight.current?.key === key) return flight.current.promise;
		const promise = (async () => {
			try {
				let storage: Storage;
				try { storage = window.localStorage; } catch { throw new HandoffStorageError(); }
				const value = await acceptOrchestratorHandoff({ projectId, sourceSessionId, hostId }, storage);
				if (active.current === key) setLast({ key, value });
				return value;
			} catch (error) {
				throw new Error(error instanceof HandoffStorageError ? t("chat.draft.handoffStorage") : t("chat.handoff.failed", { reason: apiErrorMessage(error) }));
			}
		})();
		flight.current = { key, promise };
		const clear = () => { if (flight.current?.promise === promise) flight.current = undefined; };
		void promise.then(clear, clear);
		return promise;
	}, [key, projectId, sourceSessionId, hostId, t]);
	return { request, receipt: last?.key === key ? last.value : initial };
}
