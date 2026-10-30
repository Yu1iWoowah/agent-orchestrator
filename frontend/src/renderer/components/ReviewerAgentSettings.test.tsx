import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { components } from "../../api/schema";
import { ReviewerAgentSettings } from "./ReviewerAgentSettings";
import { reviewerAgentSettingsQueryKey } from "../lib/reviewer-agent-settings";

type State = components["schemas"]["ReviewerAgentSettingsResponse"];
const api = vi.hoisted(() => ({ get: vi.fn(), patch: vi.fn() }));
vi.mock("../lib/host-clients", () => ({ clientForSessionHost: () => ({ GET: api.get, PATCH: api.patch }) }));
vi.mock("../lib/api-client", async (original) => ({ ...await original<typeof import("../lib/api-client")>(), getApiBaseUrl: () => "http://127.0.0.1:1", hasTrustedApiBaseUrl: () => true, subscribeApiBaseUrl: () => () => {} }));
vi.mock("react-i18next", () => ({ useTranslation: () => ({ t: (key: string) => key }) }));

const initial: State = { reviewId: "stable-review", harness: "codex", revision: 2, settings: { model: "current-model", effort: "low", fastMode: "off" }, activePass: false, applyMode: "next-pass", effectiveState: "unknown", reason: "reviewer_effective_unavailable" };
const clients: QueryClient[] = [];
beforeEach(() => { api.get.mockReset().mockResolvedValue({ data: initial }); api.patch.mockReset(); });
afterEach(() => { cleanup(); for (const client of clients.splice(0)) client.clear(); });

function setup() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } }); clients.push(client);
	render(<QueryClientProvider client={client}><ReviewerAgentSettings reviewId="stable-review" /></QueryClientProvider>);
	return client;
}

it("keeps unaccepted choices in the draft, exposes conflict and retries the same captured intent", async () => {
	let complete: ((value: { error: { code: string; message: string; requestId: string } }) => void) | undefined;
	api.patch.mockImplementationOnce(() => new Promise((resolve) => { complete = resolve; }));
	const client = setup();
	await waitFor(() => expect(screen.getByRole("button", { name: "reviewerSettings.edit" })).toBeEnabled());
	fireEvent.click(screen.getByRole("button", { name: "reviewerSettings.edit" }));
	expect(screen.getByRole("combobox", { name: "agentSettings.fast", exact: true })).toHaveValue("off");
	fireEvent.change(screen.getByLabelText("agentSettings.model"), { target: { value: "draft-model" } });
	fireEvent.click(screen.getByRole("button", { name: "reviewerSettings.pin" }));
	await waitFor(() => expect(api.patch).toHaveBeenCalledTimes(1));
	expect(client.getQueryData<State>(reviewerAgentSettingsQueryKey("stable-review"))?.settings?.model).toBe("current-model");
	expect(screen.getByText(/current-model ·/)).toBeInTheDocument();
	await act(async () => { complete?.({ error: { code: "AGENT_SETTINGS_CONFLICT", message: "active pass", requestId: "review-request" } }); });
	await waitFor(() => expect(screen.getByText(/review-request/)).toBeInTheDocument());
	expect(screen.getByLabelText("agentSettings.model")).toHaveValue("draft-model");
	const first = api.patch.mock.calls[0];
	api.patch.mockResolvedValue({ error: { code: "AGENT_SETTINGS_CONFLICT", message: "active pass", requestId: "retry-request" } });
	fireEvent.click(screen.getByRole("button", { name: "reviewerSettings.pin" }));
	await waitFor(() => expect(api.patch).toHaveBeenCalledTimes(2));
	expect(api.patch.mock.calls[1]?.[0]).toEqual(first?.[0]);
	for (const part of ["params", "headers", "body"]) expect(api.patch.mock.calls[1]?.[1]?.[part]).toEqual(first?.[1]?.[part]);
	expect(first?.[0]).toBe("/api/v1/reviews/{reviewId}/agent-settings");
	expect(first?.[1]).toMatchObject({ params: { path: { reviewId: "stable-review" } }, body: { expectedRevision: 2, settings: { model: "draft-model" } } });
});

it("shows the accepted pin without waiting for a background refresh and does not claim it applied", async () => {
	api.get.mockResolvedValueOnce({ data: initial }).mockImplementation(() => new Promise(() => {}));
	api.patch.mockResolvedValue({ data: { id: "pin", reviewId: "stable-review", revision: 3, phase: "pinned", applyMode: "next-pass", target: { model: "accepted-model", effort: "", fastMode: "off" } } });
	setup();
	await waitFor(() => expect(screen.getByRole("button", { name: "reviewerSettings.edit" })).toBeEnabled());
	fireEvent.click(screen.getByRole("button", { name: "reviewerSettings.edit" }));
	fireEvent.change(screen.getByLabelText("agentSettings.model"), { target: { value: "accepted-model" } });
	fireEvent.click(screen.getByRole("button", { name: "reviewerSettings.pin" }));
	await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
	expect(screen.getByText(/accepted-model ·/)).toBeInTheDocument();
	expect(screen.getByText("reviewerSettings.effectiveUnknown")).toBeInTheDocument();
});

it("blocks edits during an active pass without changing its provider or account", async () => {
	api.get.mockResolvedValue({ data: { ...initial, activePass: true } });
	setup();
	await waitFor(() => expect(screen.getByText("reviewerSettings.activePass")).toBeInTheDocument());
	expect(screen.getByRole("button", { name: "reviewerSettings.edit" })).toBeDisabled();
	expect(screen.getByText("reviewerSettings.effectiveUnknown")).toBeInTheDocument();
	expect(api.patch).not.toHaveBeenCalled();
});

it("retains a draft when another actor changes the durable preference", async () => {
	const client = setup();
	await waitFor(() => expect(screen.getByRole("button", { name: "reviewerSettings.edit" })).toBeEnabled());
	fireEvent.click(screen.getByRole("button", { name: "reviewerSettings.edit" }));
	fireEvent.change(screen.getByLabelText("agentSettings.model"), { target: { value: "my-draft" } });
	act(() => { client.setQueryData(reviewerAgentSettingsQueryKey("stable-review"), { ...initial, revision: 3, settings: { ...initial.settings, model: "external-choice" } }); });
	expect(screen.getByLabelText("agentSettings.model")).toHaveValue("my-draft");
	fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
	await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
	expect(api.patch).not.toHaveBeenCalled();
});
