import {
	conflictingSettings,
	ProjectSettingsConflict,
	projectDraftConflictPaths,
	rebaseSettings,
	settingsExpected,
} from "../lib/project-settings-patch";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ProjectSettingsRow, ProjectSettingsSection as SettingsGroup, ProjectSettingsValueRow } from "@aoagents/product-ui";
import { TriangleAlert } from "lucide-react";
import { useEffect, useRef } from "react";
import { useTranslation } from "react-i18next";
import { useCloudCp } from "../hooks/useCloudCp";
import { useCloudOffering } from "../hooks/useCloudOffering";
import { useCoderTemplates } from "../hooks/useCoderTemplates";
import { useCoderTemplateParameters } from "../hooks/useCoderTemplateParameters";
import { changedParameterOverrides, parameterOverridesUpdate } from "../lib/coder-template-parameters";
import { CoderTemplateOptions } from "./CoderTemplateOptions";
import { useOrgCoderConfig } from "../hooks/useOrgCoderConfig";
import { useProviderConnections } from "../hooks/useProviderConnections";
import { cloudProjectsQueryKey } from "../hooks/useWorkspaceQuery";
import { CLOUD_AGENT_PROVIDERS, connectedCredentialType, credentialModelScope } from "../lib/cloud-agents";
import { agentLabel } from "../lib/agent-options";
import { coderWorkspaceNamePrefixFor, isValidCoderWorkspaceNamePrefix } from "../lib/coder-workspace-name";
import { CODER_PROVIDER, projectSandboxProvider } from "../lib/cloud-offering";
import { isStandaloneCloudProject, projectAdditionalRepos, projectConfigText } from "../lib/cloud-project-repos";
import type { CloudCpProject, CloudCpProjectCoderConfig } from "../lib/cloud-cp";
import {
	CLOUD_SETTINGS_ATOMIC_PATHS,
	cloudProjectSettingsDraft,
	cloudProjectSettingsPatch,
	type CloudProjectRoleDraft,
	type CloudProjectSettingsDraft,
} from "../lib/cloud-project-settings";
import { AgentAvatar } from "./AgentAvatar";
import type { ProjectSettingsSaveState, ProjectSettingsSection as SettingsSection } from "./ProjectSettingsForm";
import { ProjectSettingsEditor, type ProjectAgentPickerProps, type ProjectSettingsDraft } from "./ProjectSettingsEditor";
import { AgentSelectMenuItem } from "./settings/AgentSelectMenuItem";
import { SettingsOptionMenu } from "./settings/SettingsOptionMenu";
import { SettingsInlineInput, SettingsRow } from "./settings/SettingsRow";

export const cloudProjectSettingsQueryKey = (baseUrl: string, orgId: string, projectId: string) =>
	["cloud-project-settings", baseUrl, orgId, projectId] as const;

export function CloudProjectSettingsAdapter({
	projectId,
	cloudOrgId,
	section = "general",
	onSaveState,
}: {
	projectId: string;
	cloudOrgId: string;
	section?: SettingsSection;
	onSaveState?: (state: ProjectSettingsSaveState) => void;
}) {
	const { t } = useTranslation();
	const { client, ready, baseUrl } = useCloudCp();
	const query = useQuery({
		queryKey: cloudProjectSettingsQueryKey(baseUrl, cloudOrgId, projectId),
		enabled: ready,
		retry: false,
		queryFn: ({ signal }) => client.getProject(cloudOrgId, projectId, { signal }).then((response) => response.project),
	});
	const loadError = !ready
		? t("settings.cloudProject.signIn")
		: query.isError
			? query.error instanceof Error
				? query.error.message
				: t("settings.project.loadFailed")
			: undefined;
	useEffect(() => {
		if (loadError)
			onSaveState?.({
				phase: "failed",
				error: loadError,
				retry: ready
					? () => {
							void query.refetch({ cancelRefetch: false });
						}
					: undefined,
			});
	}, [loadError, onSaveState, ready, query.refetch]);
	if (loadError)
		return (
			<p className="px-(--settings-row-inset) text-sm text-error" role="alert">
				{loadError}
			</p>
		);
	if (!query.data) return <p className="px-(--settings-row-inset) text-sm text-settings-muted">{t("settings.project.loading")}</p>;
	return <CloudSettingsAdapter key={`${cloudOrgId}/${projectId}`} project={query.data} section={section} onSaveState={onSaveState} />;
}

function CloudSettingsAdapter({
	project,
	section,
	onSaveState,
}: {
	project: CloudCpProject;
	section: SettingsSection;
	onSaveState?: (state: ProjectSettingsSaveState) => void;
}) {
	const { t } = useTranslation();
	const { client, baseUrl } = useCloudCp();
	const connections = useProviderConnections();
	const opencodeCredential = connectedCredentialType(connections.data, "opencode");
	const queryClient = useQueryClient();
	const baseline = useRef(project);
	// Coder settings belong only to a project that runs on Coder: the provider
	// stored on the project, else the org's default, and only while the org's
	// offering includes coder. Any other project (and every B2C org) sees none.
	const offering = useCloudOffering();
	const usesCoder = offering.hasCoder && projectSandboxProvider(project.config, offering) === CODER_PROVIDER;
	const standalone = isStandaloneCloudProject(project);
	const additionalRepos = projectAdditionalRepos(project.config);
	const snapshot = (value: CloudCpProject) => ({
		displayName: value.displayName,
		defaultBranch: value.defaultBranch,
		config: value.config,
	});
	const accept = (latest: CloudCpProject) => {
		baseline.current = latest;
		queryClient.setQueryData(cloudProjectSettingsQueryKey(baseUrl, project.orgId, project.id), latest);
	};
	const save = async (values: ProjectSettingsDraft) => {
		const patch = cloudProjectSettingsPatch(cloudProjectSettingsDraft(baseline.current), toCloudDraft(values));
		if (!Object.keys(patch).length) return { values: toEditorDraft(cloudProjectSettingsDraft(baseline.current)) };
		const expected = settingsExpected(patch, snapshot(baseline.current), CLOUD_SETTINGS_ATOMIC_PATHS);
		const latest = await client.getProject(project.orgId, project.id);
		const paths = conflictingSettings(patch, expected, snapshot(latest.project), CLOUD_SETTINGS_ATOMIC_PATHS);
		if (paths.length) throw new ProjectSettingsConflict(paths);
		const response = await client.updateProjectSettings(project.orgId, project.id, patch);
		accept(response.project);
		void queryClient.invalidateQueries({ queryKey: cloudProjectsQueryKey });
		return { values: toEditorDraft(cloudProjectSettingsDraft(response.project)) };
	};
	const reload = async (draft: ProjectSettingsDraft, paths: string[]) => {
		const before = toEditorDraft(cloudProjectSettingsDraft(baseline.current));
		const response = await client.getProject(project.orgId, project.id);
		const latest = toEditorDraft(cloudProjectSettingsDraft(response.project));
		accept(response.project);
		return { values: rebaseSettings(before, draft, latest, projectDraftConflictPaths(paths)), baseline: latest };
	};

	return (
		<ProjectSettingsEditor
			initialValues={toEditorDraft(cloudProjectSettingsDraft(project))}
			section={section}
			// A standalone project has no repository, so no branches or pull requests to configure.
			capabilities={{
				workflow: !standalone,
				sessionPrefix: true,
				intake: false,
				reviewer: true,
				requiredAgents: false,
				nameLimit: 120,
				requiredBranch: true,
				runtimeDefaults: true,
				lockedAgents: true,
			}}
			details={[
				{ label: t("settings.project.id"), value: project.id },
				{
					label: t("settings.project.kind"),
					value: standalone
						? t("settings.project.kind.cloudStandalone", { defaultValue: "Cloud, without a repository" })
						: t("settings.project.kind.cloud"),
				},
				// A standalone project's repository URL is only a placeholder.
				standalone
					? {
							label: t("settings.project.repo"),
							value: t("settings.cloudProject.noRepository", { defaultValue: "No repository" }),
						}
					: { label: t("settings.project.repo"), value: project.repositoryUrl, href: project.repositoryUrl },
				...additionalRepos.map((repo, index) => ({
					label: t("settings.cloudProject.additionalRepositoryN", {
						defaultValue: "Additional repository {{number}}",
						number: index + 1,
					}),
					value: repo.url,
					href: repo.url,
				})),
			]}
			generalExtra={
				usesCoder
					? (draft, patch) => (
							<CloudProjectCoderSettings
								project={project}
								value={draft.coder ?? { parameters: {}, workspaceNamePrefix: "" }}
								onChange={(coder) => patch({ coder })}
							/>
						)
					: undefined
			}
			validateExtra={(draft) =>
				usesCoder && !isValidCoderWorkspaceNamePrefix(draft.coder?.workspaceNamePrefix?.trim() ?? "")
					? t("coder.workspacePrefix.invalid")
					: undefined
			}
			modelScope={(agent) => (agent === "opencode" && opencodeCredential ? credentialModelScope(opencodeCredential) : "")}
			// Without a project reviewer, Cloud reviews with the session's agent: the worker's.
			defaultReviewer={(draft) => draft.workerAgent}
			autoReviewDescription={t("settings.cloudProject.autoReviewDescription")}
			renderAgent={(props) => <CloudAgentPicker {...props} />}
			save={save}
			reload={reload}
			onSaveState={onSaveState}
		/>
	);
}

/** The coder config the control plane stores on a project (config.coder). */
function coderConfigOf(project: CloudCpProject): CloudCpProjectCoderConfig | undefined {
	const coder = project.config?.coder;
	return coder && typeof coder === "object" ? (coder as CloudCpProjectCoderConfig) : undefined;
}

/** The project's Coder template, fixed at creation and shown read-only. Rendered only for a Coder project. */
export function CloudProjectCoderSettings({
	project,
	value,
	onChange,
}: {
	project: CloudCpProject;
	value: NonNullable<ProjectSettingsDraft["coder"]>;
	onChange: (value: NonNullable<ProjectSettingsDraft["coder"]>) => void;
}) {
	const { t } = useTranslation();
	const coder = coderConfigOf(project);
	const templateId = coder?.templateId?.trim() ?? "";
	const { templates, isLoading: templatesLoading } = useCoderTemplates(project.orgId, templateId !== "");
	const template = templates.find((candidate) => candidate.id === templateId);
	// The template's name, with its id secondary; the bare id when the list
	// can't load or no longer has it.
	const templateName = template ? template.displayName || template.name : "";
	// No project template only blocks sessions for a bring-your-own-Coder org
	// without an org-default template (the control plane's
	// coder_template_required). Otherwise the deployment or org default applies,
	// and non-Coder providers need no template at all.
	const orgCoder = useOrgCoderConfig(true, project.orgId);
	const orgCoderConfig = orgCoder.data as { templateId?: string; defaultTemplateId?: string } | null | undefined;
	const orgDefaultTemplateId = (orgCoderConfig?.templateId ?? orgCoderConfig?.defaultTemplateId ?? "").trim();
	const templateRequired = templateId === "" && orgCoderConfig != null && orgDefaultTemplateId === "";
	return (
		<SettingsGroup title={t("settings.project.coderTitle")} grouped>
			<ProjectSettingsRow label={t("settings.project.coderTemplate")}>
				{templateId !== "" ? (
					templateName ? (
						<span className="settings-row-value flex min-w-0 items-baseline justify-end gap-2" title={templateId}>
							<span className="shrink-0">{templateName}</span>
							<span className="min-w-0 truncate font-mono text-xs text-settings-muted" data-testid="coder-template-id">
								{templateId}
							</span>
						</span>
					) : (
						<span className="settings-row-value font-mono" title={templateId}>
							{templatesLoading ? "…" : templateId}
						</span>
					)
				) : templateRequired ? (
					<span className="settings-row-value flex items-center gap-1.5 text-error" role="alert">
						<TriangleAlert className="size-4 shrink-0" aria-hidden="true" />
						{t("settings.project.coderTemplateMissing")}
					</span>
				) : (
					<span className="settings-row-value">{orgCoder.isLoading ? "…" : t("settings.project.coderTemplateDefault")}</span>
				)}
			</ProjectSettingsRow>
			{coder?.size ? <ProjectSettingsValueRow label={t("settings.project.coderSize")} value={coder.size} /> : null}
			{/* Workspace naming is a bring-your-own-Coder control; an existing
		    prefix stays visible so it can still be cleared. */}
			{orgCoderConfig != null || (coder?.workspaceNamePrefix?.trim() ?? "") !== "" ? (
				<CoderWorkspaceNamePrefixRow
					project={project}
					value={value.workspaceNamePrefix}
					onChange={(workspaceNamePrefix) => onChange({ ...value, workspaceNamePrefix })}
				/>
			) : null}
			{templateId !== "" ? (
				<CoderTemplateParametersSettings
					project={project}
					templateId={templateId}
					value={value.parameters}
					onChange={(parameters) => onChange({ ...value, parameters })}
				/>
			) : null}
		</SettingsGroup>
	);
}

/** Coder controls share the Editor draft and never write independently. */
function CoderTemplateParametersSettings({
	project,
	templateId,
	value,
	onChange,
}: {
	project: CloudCpProject;
	templateId: string;
	value: Record<string, string>;
	onChange: (value: Record<string, string>) => void;
}) {
	const { t } = useTranslation();
	const { parameters } = useCoderTemplateParameters(project.orgId, templateId);
	return (
		<div className="settings-content-row">
			<CoderTemplateOptions
				orgId={project.orgId}
				templateId={templateId}
				overrides={changedParameterOverrides(parameters, value)}
				onChange={(draft) => onChange(parameterOverridesUpdate(parameters, value, draft) ?? value)}
				repositoryUrls={
					isStandaloneCloudProject(project)
						? []
						: [project.repositoryUrl, ...projectAdditionalRepos(project.config).map((repo) => repo.url)]
				}
				projectWorkingDirectory={projectConfigText(project.config, "workingDirectory")}
				footer={<p className="text-xs text-settings-muted">{t("coder.templateOptions.settingsHint")}</p>}
			/>
		</div>
	);
}

function CoderWorkspaceNamePrefixRow({
	project,
	value,
	onChange,
}: {
	project: CloudCpProject;
	value: string;
	onChange: (value: string) => void;
}) {
	const { t } = useTranslation();
	const invalid = !isValidCoderWorkspaceNamePrefix(value.trim());
	return (
		<SettingsRow
			label={t("coder.workspacePrefix.label")}
			description={
				invalid ? (
					<span className="text-error" role="alert">
						{t("coder.workspacePrefix.invalid")}
					</span>
				) : (
					t("coder.workspacePrefix.settingsHint")
				)
			}
		>
			<SettingsInlineInput
				id="coderWorkspaceNamePrefix"
				label={t("coder.workspacePrefix.label")}
				value={value}
				placeholder={coderWorkspaceNamePrefixFor(project.displayName)}
				onChange={onChange}
			/>
		</SettingsRow>
	);
}

function CloudAgentPicker({ role, draft, value, disabled, onChange }: ProjectAgentPickerProps) {
	const { t } = useTranslation();
	// The agent is fixed in Cloud; show the one a reviewer actually runs with.
	const shown = value || (role === "reviewer" ? draft.workerAgent : "");
	return (
		<SettingsOptionMenu
			aria-label={`${t(`settings.models.${role}Role`)} ${t("settings.project.agent").toLocaleLowerCase()}`}
			disabled={disabled}
			value={shown}
			placeholder={t("settings.cloudProject.sessionSelection")}
			options={[
				{
					value: "",
					label: t(role === "reviewer" ? "settings.cloudProject.sessionAgent" : "settings.cloudProject.sessionSelection"),
				},
				...CLOUD_AGENT_PROVIDERS.map((agent) => ({
					value: agent,
					label: agentLabel(agent),
					icon: <AgentAvatar provider={agent} className="size-icon-lg" decorative />,
				})),
			]}
			triggerClassName="w-fit"
			menuClassName="settings-agent-menu-surface"
			menuItemClassName="settings-agent-menu-item"
			renderMenuItem={(option, selected) => (
				<AgentSelectMenuItem agentId={option.value || undefined} label={option.label} selected={selected} />
			)}
			onChange={onChange}
		/>
	);
}

function toEditorDraft(values: CloudProjectSettingsDraft): ProjectSettingsDraft {
	return {
		displayName: values.displayName,
		defaultBranch: values.defaultBranch,
		autoReview: values.autoReview,
		coder: values.coder,
		sessionPrefix: values.sessionPrefix,
		intakeEnabled: false,
		intakeRepo: "",
		intakeAssignee: "",
		workerAgent: values.worker.agent,
		orchestratorAgent: values.orchestrator.agent,
		reviewerHarness: values.reviewer.agent,
		workerModel: values.worker.agentConfig.model,
		orchestratorModel: values.orchestrator.agentConfig.model,
		reviewerModel: values.reviewer.agentConfig.model,
		workerMode: values.worker.agentConfig.mode,
		orchestratorMode: values.orchestrator.agentConfig.mode,
		reviewerMode: values.reviewer.agentConfig.mode,
		workerEffort: values.worker.agentConfig.effort,
		orchestratorEffort: values.orchestrator.agentConfig.effort,
		reviewerEffort: values.reviewer.agentConfig.effort,
		workerPermissions: values.worker.agentConfig.permissions,
		orchestratorPermissions: values.orchestrator.agentConfig.permissions,
		reviewerPermissions: values.reviewer.agentConfig.permissions,
	};
}

function toCloudDraft(values: ProjectSettingsDraft): CloudProjectSettingsDraft {
	const role = (name: "worker" | "orchestrator" | "reviewer"): CloudProjectRoleDraft => {
		const agent = name === "reviewer" ? values.reviewerHarness : values[`${name}Agent`];
		const model = values[`${name}Model`];
		const mode = values[`${name}Mode`];
		const effort = values[`${name}Effort`];
		const permissions = values[`${name}Permissions`];
		const provider = agent === "" ? "" : CLOUD_AGENT_PROVIDERS.find((provider) => provider === agent);
		if (provider === undefined) throw new Error("Unsupported Cloud agent");
		if (mode !== "" && mode !== "plan" && mode !== "ask") throw new Error("Unsupported Cloud mode");
		if (effort !== "" && effort !== "low" && effort !== "medium" && effort !== "high" && effort !== "xhigh" && effort !== "max")
			throw new Error("Unsupported Cloud effort");
		if (
			permissions !== "" &&
			permissions !== "default" &&
			permissions !== "auto" &&
			permissions !== "accept-edits" &&
			permissions !== "bypass-permissions"
		)
			throw new Error("Unsupported Cloud permissions");
		return { agent: provider, agentConfig: { model, mode, effort, permissions } };
	};
	return {
		displayName: values.displayName,
		defaultBranch: values.defaultBranch,
		sessionPrefix: values.sessionPrefix,
		autoReview: values.autoReview,
		coder: values.coder ?? { parameters: {}, workspaceNamePrefix: "" },
		worker: role("worker"),
		orchestrator: role("orchestrator"),
		reviewer: role("reviewer"),
	};
}
