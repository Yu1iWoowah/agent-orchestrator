import { ProjectSettingsConflict, rebaseSettings, settingsExpected, settingsPatch, settingsSaveError } from "../lib/project-settings-patch";
import { SettingsSaveFooter } from "./settings/SettingsSaveFooter";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Eye, EyeOff, FileCode2, Plus, Trash2 } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { apiClient, apiErrorCode, apiErrorDetails, apiErrorMessage } from "../lib/api-client";
import { workspaceQueryKey } from "../hooks/useWorkspaceQuery";
import type { ProjectSettingsSaveState } from "./ProjectSettingsForm";
import { Button } from "./ui/button";
import { ProjectSettingsSection } from "@aoagents/product-ui";

type Row = { name: string; value: string; visible: boolean };

const rowsFromEnv = (env?: Record<string, string>): Row[] =>
	Object.entries(env ?? {}).map(([name, value]) => ({ name, value, visible: false }));

function parsePastedEnv(text: string): { rows: Row[]; invalidLine?: number } {
	const rows: Row[] = [];
	const names = new Set<string>();
	for (const [index, source] of text.split(/\r?\n/).entries()) {
		const line = source.trim();
		if (!line || line.startsWith("#")) continue;
		const match = /^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$/.exec(line);
		if (!match) return { rows: [], invalidLine: index + 1 };
		const name = match[1];
		const folded = name.toUpperCase();
		if (folded.startsWith("AO_") || names.has(folded)) return { rows: [], invalidLine: index + 1 };
		names.add(folded);
		let value = match[2].trim();
		if (value.startsWith('"') || value.startsWith("'")) {
			const quoted = /^(['"])(.*)\1(?:\s*#.*)?$/.exec(value);
			if (!quoted) return { rows: [], invalidLine: index + 1 };
			value = quoted[2];
			if (quoted[1] === '"') value = value.replace(/\\n/g, "\n").replace(/\\r/g, "\r");
		} else {
			value = value.replace(/\s+#.*$/, "").trimEnd();
		}
		if (value.includes("\0")) return { rows: [], invalidLine: index + 1 };
		rows.push({ name, value, visible: false });
	}
	return { rows };
}

export function ProjectEnvironmentSettings({ projectId, onSaveState }: { projectId: string; onSaveState?: (state: ProjectSettingsSaveState) => void }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const query = useQuery({
		queryKey: ["project", projectId],
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/projects/{id}", { params: { path: { id: projectId } } });
			if (error) throw new Error(apiErrorMessage(error));
			if (data?.status !== "ok" || "resolveError" in data.project) throw new Error(t("settings.project.degraded"));
			return data.project;
		},
	});
	if (query.isLoading) return <div className="project-settings-form" aria-busy="true" aria-label={t("settings.project.loading")}><section><div className="settings-grouped-rows flex w-full flex-col"><div className="settings-row-bar" /></div></section></div>;
	if (query.isError || !query.data) return <div className="flex flex-col items-start gap-3 px-(--settings-row-inset)"><p role="alert" className="text-sm text-error">{query.error instanceof Error ? query.error.message : t("settings.project.loadFailed")}</p><Button type="button" variant="ghost" size="sm" className="text-settings-muted hover:text-settings-label" onClick={() => void query.refetch()}>{t("settings.project.retry")}</Button></div>;
	return <VariablesEditor key={projectId} projectId={projectId} initial={query.data.config?.env} onSaveState={onSaveState} onSaved={() => {
		void queryClient.invalidateQueries({ queryKey: ["project", projectId] });
		void queryClient.invalidateQueries({ queryKey: workspaceQueryKey });
	}} />;
}

function VariablesEditor({ projectId, initial, onSaveState, onSaved }: {
	projectId: string;
	initial?: Record<string, string>;
	onSaveState?: (state: ProjectSettingsSaveState) => void;
	onSaved: () => void;
}) {
	const { t } = useTranslation();
	const baseline = useRef(initial ?? {});
	const queryClient = useQueryClient();
	const [reloading, setReloading] = useState(false);
	const [rows, setRows] = useState(() => rowsFromEnv(initial));
	const [saved, setSaved] = useState(() => JSON.stringify(initial ?? {}));
	const [error, setError] = useState<string | null>(null);
	const [savedAt, setSavedAt] = useState(false);
	const [activeTab, setActiveTab] = useState<"variables" | "paste">("variables");
	const [pasteText, setPasteText] = useState("");
	const [importedCount, setImportedCount] = useState<number | null>(null);
	const filled = rows.filter(({ name, value }) => name !== "" || value !== "");
	const dirty = JSON.stringify(Object.fromEntries(filled.map(({ name, value }) => [name, value]))) !== saved || pasteText.trim() !== "";
	// The env map this draft would save, or null while a name is invalid or duplicated.
	const envDraft = (() => {
		const env = new Map<string, string>();
		const names = new Set<string>();
		for (const row of filled) {
			const folded = row.name.toUpperCase();
			if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(row.name) || names.has(folded)) return null;
			names.add(folded);
			env.set(row.name, row.value);
		}
		return Object.fromEntries(env);
	})();
	const unsaveable = dirty && (envDraft === null || pasteText.trim() !== "");
	const mutation = useMutation({
		mutationFn: async (env: Record<string, string>) => {
			const patch = settingsPatch({ config: { env: baseline.current } }, { config: { env } });
			if (!Object.keys(patch).length) return baseline.current;
			const { data, error } = await apiClient.PATCH("/api/v1/projects/{id}/settings", {
				params: { path: { id: projectId } },
				body: { ...patch, expected: settingsExpected(patch, { config: { env: baseline.current } }) },
			});
			if (error) throw settingsSaveError(apiErrorCode(error), apiErrorMessage(error), apiErrorDetails(error));
			if (!data?.project) throw new Error(t("settings.project.degraded"));
			queryClient.setQueryData(["project", projectId], data.project);
			return data.project.config?.env ?? {};
		},
		onSuccess: (env) => {
			baseline.current = env;
			setRows((current) =>
				rowsFromEnv(env).map((row) => ({ ...row, visible: current.find((old) => old.name === row.name)?.visible ?? false })),
			);
			setSaved(JSON.stringify(env));
			setSavedAt(true);
			onSaved();
		},
	});
	const resetMutation = mutation.reset;
	const loadLatest = useCallback(async () => {
		if (reloading || !envDraft || !(mutation.error instanceof ProjectSettingsConflict)) return;
		setReloading(true);
		try {
			const { data, error } = await apiClient.GET("/api/v1/projects/{id}", { params: { path: { id: projectId } } });
			if (error) throw new Error(apiErrorMessage(error));
			if (data?.status !== "ok" || "resolveError" in data.project) throw new Error(t("settings.project.degraded"));
			const latest = data.project.config?.env ?? {};
			const rebased = rebaseSettings(
				baseline.current,
				envDraft,
				latest,
				mutation.error.paths.map((path) => path.replace(/^config\.env\.?/, "")),
			);
			baseline.current = latest;
			setSaved(JSON.stringify(latest));
			setRows(rowsFromEnv(rebased));
			setError(null);
			setSavedAt(false);
			resetMutation();
			queryClient.setQueryData(["project", projectId], data.project);
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : t("settings.project.loadFailed"));
		} finally {
			setReloading(false);
		}
	}, [rows, mutation.error, projectId, queryClient, reloading, resetMutation, t]);
	useEffect(() => {
		const conflict = mutation.error instanceof ProjectSettingsConflict ? mutation.error : undefined;
		onSaveState?.({
			phase: mutation.isError ? "failed" : mutation.isPending ? "saving" : savedAt && !dirty ? "saved" : "idle",
			dirty,
			unsaveable,
			requestPending: mutation.isPending,
			error: !conflict && mutation.error instanceof Error ? mutation.error.message : undefined,
			conflictPaths: conflict?.paths,
			reload: conflict ? loadLatest : undefined,
			reloading,
		});
	}, [dirty, mutation.error, mutation.isError, mutation.isPending, onSaveState, savedAt, unsaveable, loadLatest, reloading]);

	const showEmpty = rows.length === 0 && activeTab === "variables";
	const update = (next: Row[]) => {
		setRows(next);
		setError(null);
		setSavedAt(false);
		setImportedCount(null);
	};
	const importPasted = () => {
		const parsed = parsePastedEnv(pasteText);
		if (parsed.invalidLine || parsed.rows.length === 0) {
			setError(t("settings.project.envPasteInvalid", { line: parsed.invalidLine ?? 1 }));
			return;
		}
		const next = [...rows];
		for (const row of parsed.rows) {
			const existing = next.findIndex((item) => item.name.toUpperCase() === row.name.toUpperCase());
			if (existing < 0) next.push(row);
			else next[existing] = { ...row, name: next[existing].name };
		}
		update(next);
		setImportedCount(parsed.rows.length);
		setPasteText("");
		setActiveTab("variables");
	};
	// A draft that cannot be saved must still tell the dialog, or a pending close waits forever.
	const reject = (message: string) => {
		setError(message);
		onSaveState?.({ phase: "failed", dirty, requestPending: false, error: message });
	};
	const save = () => {
		if (mutation.isPending || !dirty) return;
		if (activeTab === "paste" && pasteText.trim()) {
			reject(t("settings.project.envPastePending"));
			return;
		}
		const env = new Map<string, string>();
		const names = new Set<string>();
		for (const row of filled) {
			const name = row.name;
			const folded = name.toUpperCase();
			if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(name) || names.has(folded)) {
				reject(t("settings.project.envInvalid"));
				return;
			}
			names.add(folded);
			env.set(name, row.value);
		}
		setError(null);
		mutation.mutate(Object.fromEntries(env));
	};

	return <form id="project-settings-form" className="project-settings-form flex min-h-full flex-col gap-(--size-settings-section-inner-gap)" onSubmit={(event) => { event.preventDefault(); save(); }}><fieldset className="contents" disabled={mutation.isPending || reloading}>
		<p className="px-(--settings-row-inset) text-sm leading-5 text-settings-muted">{t("settings.project.environmentDescription")}</p>
		{showEmpty ? <ProjectSettingsSection title={t("settings.project.environmentVariables")} titleHidden grouped>
			<div className="settings-row-bar">
				<span className="text-sm leading-5 text-settings-label">{t("settings.project.environmentEmptyTitle")}</span>
				<div className="flex items-center gap-2">
					<Button onClick={() => { setActiveTab("paste"); setError(null); }} type="button" variant="ghost" size="sm" className="text-settings-muted hover:text-settings-label"><FileCode2 aria-hidden="true" />{t("settings.project.pasteVariables")}</Button>
					<Button onClick={() => update([...rows, { name: "", value: "", visible: false }])} type="button" variant="ghost" size="sm" className="text-settings-muted hover:text-settings-label"><Plus aria-hidden="true" />{t("settings.project.addVariable")}</Button>
				</div>
			</div>
		</ProjectSettingsSection> : activeTab === "variables" ? <><ProjectSettingsSection title={t("settings.project.environmentVariables")} titleHidden grouped>
			{rows.map((row, index) => <div className="settings-row-bar gap-2" key={index}>
				<input aria-label={`${t("settings.project.envName")} ${index + 1}`} className="settings-field-control h-(--size-settings-action-height) min-w-0 flex-1 rounded-md!" placeholder={t("settings.project.envName")} value={row.name} onChange={(event) => update(rows.map((item, i) => i === index ? { ...item, name: event.target.value } : item))} />
				<input aria-label={`${t("settings.project.envValue")} ${index + 1}`} autoComplete="off" className="settings-field-control h-(--size-settings-action-height) min-w-0 flex-1 rounded-md!" placeholder={t("settings.project.envValue")} type={row.visible ? "text" : "password"} value={row.value} onChange={(event) => update(rows.map((item, i) => i === index ? { ...item, value: event.target.value } : item))} />
				<Button aria-label={row.visible ? t("settings.project.hideVariable") : t("settings.project.showVariable")} className="size-7 shrink-0 p-0 text-settings-muted hover:text-foreground" onClick={() => update(rows.map((item, i) => i === index ? { ...item, visible: !item.visible } : item))} size="icon-sm" title={row.visible ? t("settings.project.hideVariable") : t("settings.project.showVariable")} type="button" variant="ghost">{row.visible ? <EyeOff className="size-3.5" aria-hidden="true" /> : <Eye className="size-3.5" aria-hidden="true" />}</Button>
				<Button aria-label={t("settings.project.removeVariable", { name: row.name || index + 1 })} className="size-7 shrink-0 p-0 text-settings-muted hover:text-destructive" onClick={() => update(rows.filter((_, i) => i !== index))} size="icon-sm" title={t("settings.project.removeVariable", { name: row.name || index + 1 })} type="button" variant="ghost"><Trash2 className="size-3.5" aria-hidden="true" /></Button>
			</div>)}
		</ProjectSettingsSection>
		<div className="flex items-center justify-end gap-2">
				<Button onClick={() => { setActiveTab("paste"); setError(null); }} type="button" variant="ghost" size="sm" className="text-settings-muted hover:text-settings-label"><FileCode2 aria-hidden="true" />{t("settings.project.pasteVariables")}</Button>
				<Button onClick={() => update([...rows, { name: "", value: "", visible: false }])} size="sm" type="button"><Plus aria-hidden="true" />{t("settings.project.addVariable")}</Button>
			</div>
		</> : <ProjectSettingsSection title={t("settings.project.pasteVariables")} grouped><div className="settings-content-row flex flex-col gap-3">
			<p className="text-control leading-4 text-settings-muted">{t("settings.project.envPasteHint")}</p>
			<textarea aria-label={t("settings.project.pasteVariables")} autoComplete="off" className="settings-field-control min-h-(--size-textarea-min) min-w-0 resize-none rounded-md! py-2.5 font-mono" id="project-env-paste" onChange={(event) => setPasteText(event.target.value)} spellCheck={false} value={pasteText} />
			<div className="flex justify-end gap-2">
				<Button onClick={() => { setActiveTab("variables"); setPasteText(""); setError(null); }} type="button" variant="ghost" size="sm" className="text-settings-muted hover:text-settings-label">{t("settings.project.envPasteCancel")}</Button>
				<Button disabled={!pasteText.trim()} onClick={importPasted} size="sm" type="button">{t("settings.project.importVariables")}</Button>
			</div>
		</div></ProjectSettingsSection>}
		{importedCount !== null && <p role="status" className="px-(--settings-row-inset) text-xs text-settings-muted">{t("settings.project.envImported", { count: importedCount })}</p>}
		{error && <p role="alert" className="px-(--settings-row-inset) text-xs text-error">{error}</p>}
		{mutation.isError && !onSaveState && <p role="alert" className="px-(--settings-row-inset) text-xs text-error">{mutation.error instanceof Error ? mutation.error.message : t("settings.project.saveFailed")}</p>}
        {!onSaveState && <SettingsSaveFooter dirty={dirty} disabled={mutation.isPending || unsaveable} />}
    </fieldset></form>;
}
