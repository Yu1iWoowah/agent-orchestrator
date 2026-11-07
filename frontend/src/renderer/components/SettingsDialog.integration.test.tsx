import { useState } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { agentReadinessQueryKey } from "../hooks/useAgentReadinessQuery";
import { agentAuthPlansQueryKey } from "../hooks/useAgentAuth";
import { workspaceQueryKey } from "../hooks/useWorkspaceQuery";
import { apiClient } from "../lib/api-client";
import { useUiStore } from "../stores/ui-store";
import { agentReadiness } from "../test/agent-readiness-fixtures";
import { CreateProjectAgentSheet } from "./CreateProjectAgentSheet";
import { SettingsDialog } from "./SettingsPageTestHarness";
import { TooltipProvider } from "./ui/tooltip";

// Routing and daemon requests are external boundaries; every modal, settings
// form, selector, and recovery action in these tests is the real component.
vi.mock("@tanstack/react-router", async (importOriginal) => ({
	...await importOriginal<typeof import("@tanstack/react-router")>(),
	useNavigate: () => vi.fn(),
}));

const project = {
	id: "proj-1", name: "Project One", kind: "single_repo", path: "/repo/project-one", repo: "",
	config: { worker: { agent: "codex" }, orchestrator: { agent: "claude-code" } },
};
let latestProject: unknown;
let catalog = { agents: [agentReadiness("claude-code", "Claude Code"), agentReadiness("codex", "Codex")] };

function RecoveryDialogs({ origin }: { origin?: "create-project" }) {
	const [open, setOpen] = useState(true);
	return (
		<>
			{origin === "create-project" && <CreateProjectAgentSheet open={open} onOpenChange={setOpen} isCreating={false} kind="single_repo" path="/repo/new-project" onSubmit={async () => undefined} />}
			<SettingsDialog />
		</>
	);
}

function renderDialogs(origin?: "create-project") {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity }, mutations: { retry: false } } });
	client.setQueryData(agentReadinessQueryKey, catalog);
	client.setQueryData(workspaceQueryKey, []);
	render(<QueryClientProvider client={client}><TooltipProvider><RecoveryDialogs origin={origin} /></TooltipProvider></QueryClientProvider>);
	return client;
}

async function openAgentManagement(label: string) {
	await userEvent.click(await screen.findByLabelText(label));
	const action = screen.queryByRole("menuitem", { name: /^Manage agents(?:…)?$/ }) ?? screen.getByRole("option", { name: /^Manage agents(?:…)?$/ });
	await userEvent.click(action);
	await screen.findByRole("textbox", { name: "Search harnesses" });
}

function requireCodexLogin(client?: QueryClient) {
	catalog = { agents: [agentReadiness("claude-code", "Claude Code"), agentReadiness("codex", "Codex", { authentication: "unauthorized" })] };
	if (client) act(() => client.setQueryData(agentReadinessQueryKey, catalog));
}

beforeEach(() => {
	latestProject = undefined;
	useUiStore.setState({ settingsModal: null });
	catalog = { agents: [agentReadiness("claude-code", "Claude Code"), agentReadiness("codex", "Codex")] };
	vi.spyOn(apiClient, "GET").mockImplementation(async (path) => {
		if (path === "/api/v1/projects/{id}") return { data: { status: "ok", project: latestProject ?? project } } as never;
		if (path === "/api/v1/agents/readiness") return { data: catalog } as never;
		if (path === "/api/v1/agents/installers") return { data: { agents: [] } } as never;
		if (path === "/api/v1/agents/install-jobs") return { data: { jobs: [] } } as never;
		if (path === "/api/v1/agents/auth-plans") return { data: { plans: [{ agentId: "codex", action: "login", available: true, launchMode: "terminal" }] } } as never;
		if (path === "/api/v1/agents/{agent}/models") return { data: { agentId: "codex", models: [], selectionMode: "text", allowCustom: true, source: "manual", fetchedAt: "2026-09-19T00:00:00Z", stale: false } } as never;
		if (path === "/api/v1/settings") return { data: { defaultSessionMode: "tui", chatHarnesses: [], cloudEnabled: false, localEnabled: true, trackerIntakeEnabled: true } } as never;
		throw new Error(`Unexpected GET ${path}`);
	});
	vi.spyOn(apiClient, "POST").mockImplementation(async (path) => {
		if (path === "/api/v1/agents/readiness/ensure") return { data: catalog } as never;
		if (path === "/api/v1/agents/codex/accounts/ensure") return { data: { accountRevision: 0, accounts: [], capabilities: {}, deviceReconciliation: { status: "verified", activeAccountVerified: false, reasonCode: "verified", retryable: false } } } as never;
		throw new Error(`Unexpected POST ${path}`);
	});
	vi.spyOn(apiClient, "PUT").mockResolvedValue({ data: { project } } as never);
	vi.spyOn(apiClient, "PATCH").mockResolvedValue({ data: { status: "ok", project } } as never);
});

afterEach(() => vi.restoreAllMocks());

describe("Settings recovery modal integration", () => {
	it("closes clean project settings without writing", async () => {
		const put = vi.spyOn(apiClient, "PUT");
		useUiStore.getState().openProjectSettings("proj-1");
		renderDialogs();
		await screen.findByRole("button", { name: "Edit Project name" });
		expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();

		await userEvent.click(screen.getByRole("button", { name: "Close settings" }));
		await waitFor(() => expect(useUiStore.getState().settingsModal).toBeNull());
		expect(put).not.toHaveBeenCalled();
	});

	it.each(["close button", "Escape"])("asks before discarding edited project settings on %s", async (dismiss) => {
		const patch = vi.spyOn(apiClient, "PATCH");
		useUiStore.getState().openProjectSettings("proj-1");
		renderDialogs();
		await userEvent.click(await screen.findByRole("button", { name: "Edit Project name" }));
		const name = screen.getByRole("textbox", { name: "Project name" });
		await userEvent.clear(name);
		await userEvent.type(name, "Renamed project");
		if (dismiss === "Escape") await userEvent.keyboard("{Escape}");
		else await userEvent.click(screen.getByRole("button", { name: "Close settings" }));
		await screen.findByRole("heading", { name: "Unsaved changes" });
		expect(patch).not.toHaveBeenCalled();
		await userEvent.click(screen.getByRole("button", { name: "Keep editing" }));
		expect(useUiStore.getState().settingsModal).not.toBeNull();
		await userEvent.click(screen.getByRole("button", { name: "Edit Project name" }));
		expect(screen.getByRole("textbox", { name: "Project name" })).toHaveValue("Renamed project");
		await userEvent.click(screen.getByRole("button", { name: "Close settings" }));
		await userEvent.click(await screen.findByRole("button", { name: "Discard changes" }));
		await waitFor(() => expect(useUiStore.getState().settingsModal).toBeNull());
		expect(patch).not.toHaveBeenCalled();
	});
	it("keeps the General/Agents draft without writing or prompting", async () => {
		const patch = vi.spyOn(apiClient, "PATCH");
		useUiStore.getState().openProjectSettings("proj-1");
		renderDialogs();
		await userEvent.click(await screen.findByRole("button", { name: "Edit Project name" }));
		const name = screen.getByRole("textbox", { name: "Project name" });
		await userEvent.clear(name);
		await userEvent.type(name, "Renamed project");
		await userEvent.keyboard("{Enter}");
		await userEvent.click(screen.getByRole("button", { name: "Agents" }));
		await screen.findByRole("button", { name: "Worker agent" });
		await userEvent.click(screen.getByRole("button", { name: "General" }));
		expect(await screen.findByText("Renamed project")).toBeInTheDocument();
		expect(screen.queryByRole("heading", { name: "Unsaved changes" })).not.toBeInTheDocument();
		expect(patch).not.toHaveBeenCalled();
	});

	it("keeps edited project settings open when saving fails", async () => {
		vi.spyOn(apiClient, "PATCH").mockResolvedValue({
			error: { code: "ORCHESTRATOR_SETTINGS_UNLAUNCHABLE", message: "Agent account provider mismatch" },
		} as never);
		useUiStore.getState().openProjectSettings("proj-1");
		renderDialogs();
		await userEvent.click(await screen.findByRole("button", { name: "Edit Project name" }));
		const name = screen.getByRole("textbox", { name: "Project name" });
		await userEvent.clear(name);
		await userEvent.type(name, "Renamed project");
		await userEvent.click(screen.getByRole("button", { name: "Save" }));

		expect(await screen.findByRole("alert")).toHaveTextContent("Agent account provider mismatch");
		expect(name).toHaveValue("Renamed project");
		expect(useUiStore.getState().settingsModal).toEqual({ scope: "project", projectId: "proj-1" });
	});

	it("visits every clean local page and closes after two seconds without any settings write", async () => {
		useUiStore.getState().openProjectSettings("proj-1");
		renderDialogs();
		await screen.findByRole("button", { name: "Edit Project name" });
		for (const section of ["Agents", "Environment", "Scripts", "General"]) {
			await userEvent.click(screen.getByRole("button", { name: section }));
			await waitFor(() => expect(screen.getByRole("button", { name: section })).toHaveAttribute("data-active", "true"));
		}
		await act(async () => {
			await new Promise((resolve) => setTimeout(resolve, 2000));
		});
		await userEvent.click(screen.getByRole("button", { name: "Close settings" }));
		expect(screen.queryByRole("heading", { name: "Unsaved changes" })).not.toBeInTheDocument();
		expect(apiClient.PATCH).not.toHaveBeenCalled();
		expect(apiClient.PUT).not.toHaveBeenCalled();
		expect(vi.mocked(apiClient.POST).mock.calls.filter((call) => call[0] === "/api/v1/orchestrators")).toEqual([]);
	});
	it("reloads a conflicting worker model, keeps other edits and uses the latest response as baseline", async () => {
		useUiStore.getState().openProjectSettings("proj-1");
		renderDialogs();
		await userEvent.click(await screen.findByRole("button", { name: "Edit Project name" }));
		fireEvent.change(screen.getByRole("textbox", { name: "Project name" }), { target: { value: "My name" } });
		await userEvent.click(screen.getByRole("button", { name: "Agents" }));
		await userEvent.click(await screen.findByRole("button", { name: "Worker model" }));
		await userEvent.type(screen.getByRole("searchbox", { name: "Search worker model" }), "my-model");
		await userEvent.click(screen.getByRole("menuitem", { name: "Use “my-model” as a custom model" }));
		vi.mocked(apiClient.PATCH).mockResolvedValueOnce({
			error: {
				code: "PROJECT_SETTINGS_CONFLICT",
				message: "settings changed",
				details: { conflicts: [{ path: "config.worker.agentConfig.model" }] },
			},
		} as never);
		await userEvent.click(screen.getByRole("button", { name: "Save" }));
		expect(await screen.findByText(/Worker · Model/)).toBeInTheDocument();
		expect(apiClient.PATCH).toHaveBeenNthCalledWith(1,"/api/v1/projects/{id}/settings",{params:{path:{id:"proj-1"}},body:{
			displayName: "My name",
			config: { worker: { agentConfig: { model: "my-model" } } },
			expected: { displayName: "Project One", config: { worker: { agentConfig: { model: null } } } },
		}});
  latestProject={...project,config:{...project.config,sessionPrefix:"external-prefix",worker:{agent:"codex",agentConfig:{model:"external-model"}},reviewers:[{harness:"claude-code"}]}};
		await userEvent.click(screen.getByRole("button", { name: "Load latest settings" }));
		await waitFor(() => expect(screen.getByRole("button", { name: "Worker model" })).toHaveTextContent("external-model"));
		expect(screen.queryByRole("button", { name: "Load latest settings" })).not.toBeInTheDocument();
		await userEvent.click(screen.getByRole("button", { name: "General" }));
		expect(await screen.findByText("My name")).toBeInTheDocument();
		expect(screen.getByText("external-prefix")).toBeInTheDocument();
		vi.mocked(apiClient.PATCH).mockResolvedValueOnce({
			data: {
				project: {
					...project,
					name: "My name",
					config: {
						...project.config,
						sessionPrefix: "external-prefix",
						worker: { agent: "codex", agentConfig: { model: "external-model" } },
						reviewers: [{ harness: "claude-code" }],
					},
				},
			},
		} as never);
		await userEvent.click(screen.getByRole("button", { name: "Save" }));
		await waitFor(() => expect(apiClient.PATCH).toHaveBeenCalledTimes(2));
		expect(apiClient.PATCH).toHaveBeenNthCalledWith(2,"/api/v1/projects/{id}/settings",{params:{path:{id:"proj-1"}},body:{
			displayName: "My name",
			expected: { displayName: "Project One" },
		}});
		await waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeDisabled());
	});

 it("does not resend reviewers changed elsewhere and accepts them from the successful response", async () => {
  useUiStore.getState().openProjectSettings("proj-1");const client=renderDialogs();
  await userEvent.click(await screen.findByRole("button",{name:"Agents"}));
  await userEvent.click(await screen.findByRole("button",{name:"Worker model"}));
  await userEvent.type(screen.getByRole("searchbox",{name:"Search worker model"}),"my-model");
  await userEvent.click(screen.getByRole("menuitem",{name:"Use “my-model” as a custom model"}));
  const externalReviewers=[{harness:"claude-code",agentConfig:{model:"external-reviewer"}}];
  vi.mocked(apiClient.PATCH).mockResolvedValueOnce({data:{project:{...project,config:{...project.config,worker:{agent:"codex",agentConfig:{model:"my-model"}},reviewers:externalReviewers}}}} as never);
  await userEvent.click(screen.getByRole("button",{name:"Save"}));
  await waitFor(()=>expect(screen.getByRole("button",{name:"Save"})).toBeDisabled());
  expect(apiClient.PATCH).toHaveBeenCalledExactlyOnceWith("/api/v1/projects/{id}/settings",{params:{path:{id:"proj-1"}},body:{config:{worker:{agentConfig:{model:"my-model"}}},expected:{config:{worker:{agentConfig:{model:null}}}}}});
  expect(client.getQueryData(["project","proj-1"])).toMatchObject({config:{reviewers:externalReviewers}});
  expect(screen.getByRole("button",{name:"Reviewer model"})).toHaveTextContent("external-reviewer");
 });

	it("leaves focus on the targeted Harness action when readiness is already cached", async () => {
		requireCodexLogin();
		const client = renderDialogs();
		act(() => {
			client.setQueryData(agentReadinessQueryKey, catalog);
			client.setQueryData(["agent-installers"], []);
			client.setQueryData(["agent-install-jobs"], []);
			client.setQueryData(agentAuthPlansQueryKey, [
				{ agentId: "codex", action: "login", available: true, launchMode: "terminal" },
			]);
			useUiStore.getState().openGlobalSettings("harness", { focusAgentId: "codex" });
		});

		const row = (await screen.findByText("Codex")).closest('[data-agent="codex"]') as HTMLElement;
		const login = await within(row).findByRole("button", { name: "Login" });
		await waitFor(() => expect(document.activeElement).toBe(login));
	});

	it("returns focus to the project selector after setup and refreshes its choices", async () => {
		requireCodexLogin();
		useUiStore.getState().openProjectSettings("proj-1");
		const client = renderDialogs();
		await userEvent.click(await screen.findByRole("button", { name: "Agents" }));
		const trigger = await screen.findByLabelText("Worker agent");
		await openAgentManagement("Worker agent");
		await screen.findByRole("tab", { name: "Account" });

		act(() => client.setQueryData(agentReadinessQueryKey, {
			agents: [agentReadiness("claude-code", "Claude Code"), agentReadiness("codex", "Codex")],
		}));
		await waitFor(() => expect(trigger).not.toHaveTextContent("Needs setup"));
		await userEvent.click(screen.getByRole("button", { name: "Back" }));

		await waitFor(() => expect(useUiStore.getState().settingsModal).toEqual({ scope: "project", projectId: "proj-1", section: undefined }));
		await userEvent.click(trigger);
		expect(await screen.findByRole("menuitem", { name: /Codex/ })).toBeInTheDocument();
		await userEvent.keyboard("{Escape}");
		expect(screen.getByRole("button", { name: "Agents" })).toHaveAttribute("data-active", "true");
		await userEvent.keyboard("{Escape}");
		await waitFor(() => expect(screen.queryByTestId("settings-page")).not.toBeInTheDocument());
	});

	it("allows pointer interaction above create-project and restores its selections and intake draft on close", async () => {
		const client = renderDialogs("create-project");
		await userEvent.click(screen.getByRole("combobox", { name: "Worker agent" }));
		await userEvent.click(await screen.findByRole("option", { name: /Codex/i }));
		await userEvent.click(screen.getByLabelText("Automatically work on assigned issues"));
		await userEvent.type(screen.getByLabelText("Assignee"), "octocat");
		requireCodexLogin(client);
		await openAgentManagement("Worker agent");

		await userEvent.click(await screen.findByRole("button", { name: "All harnesses" }));
		const search = await screen.findByRole("textbox", { name: "Search harnesses" });
		await userEvent.type(search, "Claude");
		expect(search).toHaveValue("Claude");
		await userEvent.click(screen.getByRole("button", { name: "Back" }));

		expect(await screen.findByRole("combobox", { name: "Worker agent" })).toHaveTextContent("Codex");
		expect(screen.getByLabelText("Assignee")).toHaveValue("octocat");
		expect(useUiStore.getState().settingsModal).toBeNull();
		await userEvent.keyboard("{Escape}");
		await waitFor(() => expect(screen.queryByTestId("settings-page")).not.toBeInTheDocument());
	});

	it.each(["close button", "Escape"])("keeps the real project form mounted beneath recovery and returns to its draft via %s", async (dismiss) => {
		const put = vi.spyOn(apiClient, "PUT").mockResolvedValue({ data: { status: "ok" } } as never);
		requireCodexLogin();
		useUiStore.getState().openProjectSettings("proj-1");
		renderDialogs();
		await userEvent.click(await screen.findByRole("button", { name: "Edit Project name" }));
		const name = screen.getByRole("textbox", { name: "Project name" });
		await userEvent.clear(name);
		await userEvent.type(name, "Unsaved project name");
		await userEvent.click(screen.getByRole("button", { name: "Agents" }));
		const form = document.getElementById("project-settings-form");
		await openAgentManagement("Worker agent");
		await screen.findByRole("tab", { name: "Account" });

		expect(form).not.toBeNull();
		if (dismiss === "Escape") await userEvent.keyboard("{Escape}");
		else await userEvent.click(screen.getByRole("button", { name: "Back" }));

		expect(await screen.findByRole("button", { name: "Worker agent" })).toHaveTextContent("Codex");
		expect(screen.getByRole("button", { name: "Agents" })).toHaveAttribute("data-active", "true");
		expect(document.getElementById("project-settings-form")).toBe(form);
		await userEvent.click(screen.getByRole("button", { name: "General" }));
		expect(await screen.findByRole("button", { name: "Edit Project name" })).toHaveTextContent("Unsaved project name");
		expect(useUiStore.getState().settingsModal).toEqual({ scope: "project", projectId: "proj-1" });
		await userEvent.click(screen.getByRole("button", { name: "Close settings" }));
  await userEvent.click(await screen.findByRole("button",{name:"Discard changes"}));
  expect(put).not.toHaveBeenCalled();expect(apiClient.PATCH).not.toHaveBeenCalled();
		await waitFor(() => expect(screen.queryByTestId("settings-page")).not.toBeInTheDocument());
	});
});
