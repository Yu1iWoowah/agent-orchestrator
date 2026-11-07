import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { ProjectEnvironmentSettings } from "./ProjectEnvironmentSettings";

const { getMock, putMock, patchMock } = vi.hoisted(() => ({ getMock: vi.fn(), putMock: vi.fn(), patchMock: vi.fn() }));
vi.mock("../lib/api-client", () => ({
	apiClient: { GET: getMock, PUT: putMock, PATCH: patchMock },
	apiErrorMessage: () => "request failed",
	apiErrorCode: (error: { code?: string }) => error?.code,
	apiErrorDetails: (error: { details?: unknown }) => error?.details,
}));

beforeEach(() => {
	getMock.mockReset().mockResolvedValue({ data: { status: "ok", project: { id: "p", name: "Example", config: { env: { EXISTING: "old" }, autoReview: true } } } });
	putMock.mockReset().mockResolvedValue({ data: { status: "ok" } });
	patchMock
		.mockReset()
		.mockImplementation(async (_path: string, request: { body: { config?: { env?: Record<string, string | null> } } }) => {
			const env: Record<string, string> = { EXISTING: "old" };
			for (const [key, value] of Object.entries(request.body.config?.env ?? {})) {
				if (value === null) delete env[key];
				else env[key] = value;
			}
			return { data: { project: { id: "p", name: "Example", config: { env, autoReview: true } } } };
		});
});

it("saves validated variables without dropping other project settings", async () => {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	render(<QueryClientProvider client={client}><ProjectEnvironmentSettings projectId="p" /></QueryClientProvider>);
	const value = await screen.findByLabelText("Value 1");
	expect(value).toHaveAttribute("type", "password");
	await userEvent.clear(value);
	await userEvent.type(value, "new");
	await userEvent.click(screen.getByRole("button", { name: "Add variable" }));
	await userEvent.type(screen.getByLabelText("Name 2"), "SECOND");
	await userEvent.type(screen.getByLabelText("Value 2"), "another");
	fireEvent.submit(document.getElementById("project-settings-form")!);
	await waitFor(() => expect(patchMock).toHaveBeenCalledOnce());
	expect(patchMock.mock.calls[0][1].body).toEqual({
		config: { env: { EXISTING: "new", SECOND: "another" } },
		expected: { config: { env: { EXISTING: "old", SECOND: null } } },
	});
});

it("imports pasted .env entries into the draft, replacing matching names without saving early", async () => {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	render(<QueryClientProvider client={client}><ProjectEnvironmentSettings projectId="p" /></QueryClientProvider>);
	await screen.findByLabelText("Value 1");
	await userEvent.click(screen.getByRole("button", { name: "Paste .env" }));
	fireEvent.change(screen.getByRole("textbox", { name: "Paste .env" }), { target: { value: "# copied config\nexisting=replaced\nexport NEW_KEY=\"hello world\"\nURL=https://example.com/?a=b" } });
	fireEvent.submit(document.getElementById("project-settings-form")!);
	expect(screen.getByRole("alert")).toHaveTextContent("Add the pasted entries to the draft before saving");
	expect(patchMock).not.toHaveBeenCalled();
	await userEvent.click(screen.getByRole("button", { name: "Add to draft" }));
	expect(patchMock).not.toHaveBeenCalled();
	expect(screen.getByLabelText("Value 1")).toHaveValue("replaced");
	expect(screen.getByLabelText("Value 2")).toHaveAttribute("type", "password");
	fireEvent.submit(document.getElementById("project-settings-form")!);
	await waitFor(() => expect(patchMock).toHaveBeenCalledOnce());
	expect(patchMock.mock.calls[0][1].body.config.env).toEqual({
		EXISTING: "replaced",
		NEW_KEY: "hello world",
		URL: "https://example.com/?a=b",
	});
});

it("rejects a pasted reserved name without partially changing the draft", async () => {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	const onSaveState = vi.fn();
	render(<QueryClientProvider client={client}><ProjectEnvironmentSettings projectId="p" onSaveState={onSaveState} /></QueryClientProvider>);
	await screen.findByLabelText("Value 1");
	await userEvent.click(screen.getByRole("button", { name: "Paste .env" }));
	fireEvent.change(screen.getByRole("textbox", { name: "Paste .env" }), { target: { value: "NEW=good\nAO_SESSION_ID=spoof" } });
	await userEvent.click(screen.getByRole("button", { name: "Add to draft" }));
	expect(screen.getByRole("alert")).toHaveTextContent("Check line 2");
	expect(screen.queryByLabelText("Value 2")).not.toBeInTheDocument();
	await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
	expect(screen.getByLabelText("Value 1")).toHaveValue("old");
	expect(patchMock).not.toHaveBeenCalled();
	expect(onSaveState).toHaveBeenLastCalledWith(expect.objectContaining({ phase: "idle", dirty: false }));
});

it("keeps an edited env draft without writing until explicitly saved", async () => {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	render(
		<QueryClientProvider client={client}>
			<ProjectEnvironmentSettings projectId="p" />
		</QueryClientProvider>,
	);
	const value = await screen.findByLabelText("Value 1");
	await userEvent.clear(value);
	await userEvent.type(value, "mine");
	await new Promise((resolve) => setTimeout(resolve, 850));
	expect(putMock).not.toHaveBeenCalled();
	expect(patchMock).not.toHaveBeenCalled();
	fireEvent.submit(document.getElementById("project-settings-form")!);
	await waitFor(() => expect(patchMock).toHaveBeenCalledOnce());
	expect(patchMock.mock.calls[0][1].body).toEqual({
		config: { env: { EXISTING: "mine" } },
		expected: { config: { env: { EXISTING: "old" } } },
	});
});

it("loads a conflicting environment value while preserving another dirty variable", async () => {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	const states = vi.fn();
	render(
		<QueryClientProvider client={client}>
			<ProjectEnvironmentSettings projectId="p" onSaveState={states} />
		</QueryClientProvider>,
	);
	fireEvent.change(await screen.findByLabelText("Value 1"), { target: { value: "mine" } });
	await userEvent.click(screen.getByRole("button", { name: "Add variable" }));
	fireEvent.change(screen.getByLabelText("Name 2"), { target: { value: "SECOND" } });
	fireEvent.change(screen.getByLabelText("Value 2"), { target: { value: "keep" } });
	patchMock.mockResolvedValueOnce({
		error: { code: "PROJECT_SETTINGS_CONFLICT", details: { conflicts: [{ path: "config.env.EXISTING" }] } },
	});
	fireEvent.submit(document.getElementById("project-settings-form")!);
	await waitFor(() => expect(states.mock.lastCall?.[0].conflictPaths).toEqual(["config.env.EXISTING"]));
	getMock.mockResolvedValue({
		data: { status: "ok", project: { id: "p", name: "Example", config: { env: { EXISTING: "external", THIRD: "fresh" } } } },
	});
	await act(async () => {
		await states.mock.lastCall?.[0].reload();
	});
	expect(screen.getByLabelText("Value 1")).toHaveValue("external");
	expect(screen.getByLabelText("Value 2")).toHaveValue("fresh");
	expect(screen.getByLabelText("Value 3")).toHaveValue("keep");
	fireEvent.submit(document.getElementById("project-settings-form")!);
	await waitFor(() => expect(patchMock).toHaveBeenCalledTimes(2));
	expect(patchMock.mock.calls[1][1].body).toEqual({
		config: { env: { SECOND: "keep" } },
		expected: { config: { env: { SECOND: null } } },
	});
});

it("PATCHes legal variable names that match JavaScript prototype members", async () => {
 const client=new QueryClient({defaultOptions:{queries:{retry:false},mutations:{retry:false}}});
 render(<QueryClientProvider client={client}><ProjectEnvironmentSettings projectId="p"/></QueryClientProvider>);
 await screen.findByLabelText("Value 1");
 for(const [index,name] of ["constructor","__proto__"].entries()) {
  await userEvent.click(screen.getByRole("button",{name:"Add variable"}));
  fireEvent.change(screen.getByLabelText(`Name ${index+2}`),{target:{value:name}});
  fireEvent.change(screen.getByLabelText(`Value ${index+2}`),{target:{value:"value"}});
 }
 fireEvent.submit(document.getElementById("project-settings-form")!);
 await waitFor(()=>expect(patchMock).toHaveBeenCalledOnce());
 expect(JSON.stringify(patchMock.mock.calls[0][1].body)).toBe('{"config":{"env":{"constructor":"value","__proto__":"value"}},"expected":{"config":{"env":{"constructor":null,"__proto__":null}}}}');
});
