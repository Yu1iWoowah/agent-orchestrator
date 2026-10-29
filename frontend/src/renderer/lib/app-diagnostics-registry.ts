import { useLayoutEffect, useSyncExternalStore } from "react";
import type { ComponentObservation } from "../../shared/app-diagnostics";

export type CommittedObservation = Omit<ComponentObservation, "revision" | "observedAt">;
export type DiagnosticRegion = { value: ComponentObservation; root: () => HTMLElement | null; list?: () => HTMLElement | null };
let active = false;
let revision = 0;
let omitted = 0;
const regions = new Map<string, DiagnosticRegion>();
const listeners = new Set<() => void>();
export const diagnosticsEnabled = () => active;
export const diagnosticCommitRevision = () => revision;
export const diagnosticRegions = () => ({ regions: [...regions.values()], omitted });
export function setDiagnosticRecording(enabled: boolean): void {
	if (active === enabled) return;
	active = enabled;
	regions.clear();
	omitted = 0;
	revision++;
	for (const listener of listeners) listener();
}
function subscribe(listener: () => void): () => void {
	listeners.add(listener);
	return () => listeners.delete(listener);
}
export function useDiagnosticCommit(
	key: string,
	sample: () => CommittedObservation,
	root: () => HTMLElement | null,
	list?: () => HTMLElement | null,
): void {
	const enabled = useSyncExternalStore(subscribe, diagnosticsEnabled, () => false);
	useLayoutEffect(() => {
		if (!enabled) return;
		if (regions.size >= 102 && !regions.has(key)) {
			omitted = Math.min(1e7, omitted + 1);
			return;
		}
		const entry: DiagnosticRegion = { value: { ...sample(), revision: ++revision, observedAt: new Date().toISOString() }, root, list };
		regions.set(key, entry);
		return () => {
			if (regions.get(key) === entry) {
				regions.delete(key);
				revision++;
			}
		};
	}, [enabled, key, sample, root, list]);
}
