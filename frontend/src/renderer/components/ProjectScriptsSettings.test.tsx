import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { terminateSessionMutationKey } from "../hooks/useTerminateSession";
import { ProjectScriptsSettings } from "./ProjectScriptsSettings";

const { getMock, putMock, patchMock, postMock } = vi.hoisted(() => ({
	getMock: vi.fn(),
	putMock: vi.fn(),
	patchMock: vi.fn(),
	postMock: vi.fn(),
}));
vi.mock("../lib/api-client", () => ({
	apiClient: { GET: getMock, PUT: putMock, PATCH: patchMock, POST: postMock },
	apiErrorMessage: () => "request failed",
	apiErrorCode: (error: { code?: string }) => error?.code,
	apiErrorDetails: (error: { details?: unknown }) => error?.details,
}));

const config = {
	postCreate: ["echo first", "echo second"],
	preRemove: ["docker compose down", "rm -rf .cache/tmp"],
	autoReview: true,
};

beforeEach(() => {
	getMock.mockReset().mockResolvedValue({ data: { status: "ok", project: { id: "p", name: "Example", kind: "single_repo", config } } });
	putMock.mockReset().mockResolvedValue({ data: { status: "ok" } });
	patchMock.mockReset().mockImplementation(async (_path: string, request: { body: { config: Record<string, unknown> } }) => {
		const first = await getMock.mock.results[0].value;
		return { data: { project: { ...first.data.project, config: { ...first.data.project.config, ...request.body.config } } } };
	});
	postMock
		.mockReset()
		.mockResolvedValue({
			data: {
				ok: true,
				cleaned: ["session-1"],
				alreadyGone: [],
				skipped: [{ sessionId: "session-2", reason: "workspace has uncommitted changes" }],
			},
		});
});

function renderScripts(client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })) {
	return render(<QueryClientProvider client={client}><ProjectScriptsSettings projectId="p" /></QueryClientProvider>);
}

it("retains drafts across tabs and PATCHes changed ordered lists with opening expected values", async () => {
	renderScripts();
	expect(await screen.findByLabelText("Step 1")).toHaveValue("echo first");
	fireEvent.change(screen.getByLabelText("Step 2"), { target: { value: "echo updated" } });
	await userEvent.click(screen.getByRole("tab", { name: "Cleanup" }));
	expect(screen.getByLabelText("Step 1")).toHaveValue("docker compose down");
	expect(screen.getByRole("button", { name: "Retry cleanup" })).toBeDisabled();
	fireEvent.change(screen.getByLabelText("Step 2"), { target: { value: "rm -rf .tmp" } });
	await userEvent.click(screen.getByRole("tab", { name: "Setup" }));
	expect(screen.getByLabelText("Step 2")).toHaveValue("echo updated");
	getMock.mockResolvedValue({ data: { status: "ok", project: { id: "p", name: "Renamed", kind: "single_repo", config: { ...config, env: { TOKEN: "latest-value" } } } } });
	fireEvent.submit(document.getElementById("project-settings-form")!);
	await waitFor(() => expect(patchMock).toHaveBeenCalledOnce());
	expect(patchMock.mock.calls[0][1].body).toEqual({
		config: {
			postCreate: ["echo first", "echo updated"],
			preRemove: ["docker compose down", "rm -rf .tmp"],
		},
		expected: { config: { postCreate: config.postCreate, preRemove: config.preRemove } },
	});
	await waitFor(() => expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled());
	await userEvent.click(screen.getByRole("tab", { name: "Cleanup" }));
	expect(screen.getByLabelText("Step 2")).toHaveValue("rm -rf .tmp");
	expect(screen.getByRole("button", { name: "Retry cleanup" })).toBeEnabled();
});

it("retries cleanup for this project and shows preserved workspaces", async () => {
	renderScripts();
	await userEvent.click(await screen.findByRole("tab", { name: "Cleanup" }));
	await userEvent.click(screen.getByRole("button", { name: "Retry cleanup" }));
	await waitFor(() => expect(postMock).toHaveBeenCalledWith("/api/v1/sessions/cleanup", { params: { query: { project: "p" } } }));
	expect(await screen.findByText(/session-2: workspace has uncommitted changes/)).toBeInTheDocument();
});

it("clears resolved archive errors after cleanup retry", async () => {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	postMock.mockResolvedValueOnce({ data: { ok: true, cleaned: ["session-1"], alreadyGone: ["session-3"], skipped: [{ sessionId: "session-2", reason: "workspace has uncommitted changes" }] } });
	for (const id of ["session-1", "session-2", "session-3"]) {
		const mutation = client.getMutationCache().build(client, {
			mutationKey: terminateSessionMutationKey,
			mutationFn: async () => { throw new Error("Workspace cleanup script failed"); },
		});
		await expect(mutation.execute({ id })).rejects.toThrow("Workspace cleanup script failed");
	}
	renderScripts(client);
	await userEvent.click(await screen.findByRole("tab", { name: "Cleanup" }));
	await userEvent.click(screen.getByRole("button", { name: "Retry cleanup" }));
	await screen.findByText("Cleaned 1; skipped 1.");
	const remaining = client.getMutationCache().findAll({ mutationKey: terminateSessionMutationKey });
	expect(remaining.map((mutation) => (mutation.state.variables as { id: string }).id)).toEqual(["session-2"]);
});

it("keeps unsaved edits and disables retry when saving fails", async () => {
	patchMock.mockResolvedValue({ error: { message: "failed" } });
	renderScripts();
	await userEvent.click(await screen.findByRole("tab", { name: "Cleanup" }));
	fireEvent.change(screen.getByLabelText("Step 1"), { target: { value: "echo fixed" } });
	fireEvent.submit(document.getElementById("project-settings-form")!);
	expect(await screen.findByRole("alert")).toHaveTextContent("request failed");
	expect(screen.getByLabelText("Step 1")).toHaveValue("echo fixed");
	expect(screen.getByRole("button", { name: "Retry cleanup" })).toBeDisabled();
	expect(postMock).not.toHaveBeenCalled();
});

it("allows scratch setup without enabling cleanup or changing its stored cleanup config", async () => {
	getMock.mockResolvedValue({ data: { status: "ok", project: { id: "p", name: "Scratch", kind: "scratch", config } } });
	renderScripts();
	fireEvent.change(await screen.findByLabelText("Step 1"), { target: { value: "echo scratch" } });
	await userEvent.click(screen.getByRole("tab", { name: "Cleanup" }));
	expect(screen.getByText(/Automatic cleanup commands are available for Git worktrees/)).toBeInTheDocument();
	expect(screen.queryByRole("button", { name: "Retry cleanup" })).not.toBeInTheDocument();
	expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
	fireEvent.submit(document.getElementById("project-settings-form")!);
	await waitFor(() => expect(patchMock).toHaveBeenCalledOnce());
	expect(patchMock.mock.calls[0][1].body.config).toEqual({ postCreate: ["echo scratch", "echo second"] });
});

it("gives local steps the same time limit field, with no limit by default for Cleanup", async () => {
	getMock.mockResolvedValue({
		data: {
			status: "ok",
			project: {
				id: "p",
				name: "Example",
				kind: "single_repo",
				config: {
					...config,
					postCreate: ["# ao-timeout: 30m\nnpm ci"],
					preRemove: ["docker compose down"],
				},
			},
		},
	});
	renderScripts();
	expect(await screen.findByLabelText("Step 1")).toHaveValue("npm ci");
	expect(screen.getByLabelText("Time limit for step 1, in minutes")).toHaveValue("30");
	await userEvent.click(screen.getByRole("tab", { name: "Cleanup" }));
	const cleanup = screen.getByLabelText("Time limit for step 1, in minutes");
	expect(cleanup).toHaveValue("");
	// No limit reads as a word, and the hint says what empty means.
	expect(cleanup).toHaveAttribute("placeholder", "none");
	expect(cleanup).toHaveAccessibleDescription("Time limit in minutes (1 to 120). Leave empty for no limit.");
	fireEvent.change(cleanup, { target: { value: "5" } });
	fireEvent.submit(document.getElementById("project-settings-form")!);
	await waitFor(() => expect(patchMock).toHaveBeenCalledOnce());
	expect(patchMock.mock.calls[0][1].body.config).not.toHaveProperty("postCreate");
	expect(patchMock.mock.calls[0][1].body.config).toMatchObject({
		preRemove: ["# ao-timeout: 5m\ndocker compose down"],
	});
});

it("shows the next step's time limit tooltip at once when moving from one field to the next", async () => {
	const user = userEvent.setup();
	renderScripts();
	const first = await screen.findByLabelText("Time limit for step 1, in minutes");
	const second = screen.getByLabelText("Time limit for step 2, in minutes");
	await user.hover(first);
	await waitFor(() => expect(first).toHaveAttribute("data-state", "delayed-open"));
	await user.unhover(first);
	await user.hover(second);
	// The fields share one tooltip provider, which skips the open delay.
	expect(second).toHaveAttribute("data-state", "instant-open");
});

it("reloads a conflicting Setup list while keeping the Cleanup draft", async () => {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	const states = vi.fn();
	render(
		<QueryClientProvider client={client}>
			<ProjectScriptsSettings projectId="p" onSaveState={states} />
		</QueryClientProvider>,
	);
	fireEvent.change(await screen.findByLabelText("Step 1"), { target: { value: "my setup" } });
	await userEvent.click(screen.getByRole("tab", { name: "Cleanup" }));
	fireEvent.change(screen.getByLabelText("Step 1"), { target: { value: "my cleanup" } });
	patchMock.mockResolvedValueOnce({
		error: { code: "PROJECT_SETTINGS_CONFLICT", details: { conflicts: [{ path: "config.postCreate" }] } },
	});
	fireEvent.submit(document.getElementById("project-settings-form")!);
	await waitFor(() => expect(states.mock.lastCall?.[0].conflictPaths).toEqual(["config.postCreate"]));
	getMock.mockResolvedValue({
		data: {
			status: "ok",
			project: { id: "p", name: "Example", kind: "single_repo", config: { ...config, postCreate: ["external setup"] } },
		},
	});
	await act(async () => {
		await states.mock.lastCall?.[0].reload();
	});
	expect(screen.getByLabelText("Step 1")).toHaveValue("my cleanup");
	await userEvent.click(screen.getByRole("tab", { name: "Setup" }));
	expect(screen.getByLabelText("Step 1")).toHaveValue("external setup");
	fireEvent.submit(document.getElementById("project-settings-form")!);
	await waitFor(() => expect(patchMock).toHaveBeenCalledTimes(2));
	expect(patchMock.mock.calls[1][1].body).toEqual({
		config: { preRemove: ["my cleanup", "rm -rf .cache/tmp"] },
		expected: { config: { preRemove: config.preRemove } },
	});
});
