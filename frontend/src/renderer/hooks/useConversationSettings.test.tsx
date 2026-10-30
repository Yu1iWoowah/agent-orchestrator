import { QueryClient, QueryClientProvider, useQuery, type InfiniteData } from "@tanstack/react-query";
import { act, render, renderHook, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import type { ConversationSnapshot } from "../types/conversation";
import { TurnSettingsBar } from "../components/chat/TurnSettingsBar";
import { AgentSettingsSubmission } from "../components/AgentSettingsSubmission";
import { describe, expect, it, vi } from "vitest";
import { chatFixtureEmpty } from "../lib/chat-fixture";

const { patch } = vi.hoisted(() => ({ patch: vi.fn() }));
vi.mock("../lib/api-client", () => ({
	apiClient: { PATCH: patch },
	apiErrorMessage: (error: unknown) => typeof error === "object" && error !== null && "message" in error && typeof error.message === "string" ? error.message : "Failed",
	apiErrorCode: (error: unknown) => typeof error === "object" && error !== null && "code" in error && typeof error.code === "string" ? error.code : undefined,
	apiErrorRequestId: (error: unknown) => typeof error === "object" && error !== null && "requestId" in error && typeof error.requestId === "string" ? error.requestId : undefined,
}));
import { conversationQueryKey, useConversationCommands } from "./useConversation";

describe("confirmed conversation permissions", () => {
	it.each(["retry", "dismiss"])("keeps rejected choices separate from confirmed values for explicit %s", async (action) => {
		let finish!: (value: unknown) => void;
		patch.mockReset().mockReturnValueOnce(new Promise((resolve) => { finish = resolve; })).mockImplementation(async (_path: string, options: { body: unknown }) => ({ data: options.body }));
		const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
		const key = conversationQueryKey("session-a");
		const initial = { pages: [{ ...chatFixtureEmpty, sessionId: "session-a", settings: { model: "committed-model", reasoningEffort: "low", fastMode: true } }], pageParams: [undefined] };
		client.setQueryData(key, initial);
		function Choice() {
			const query = useQuery<InfiniteData<ConversationSnapshot>>({ queryKey: key, queryFn: () => initial, enabled: false });
			const commands = useConversationCommands("session-a");
			return <>
				<p data-testid="confirmed-model">{query.data?.pages[0].settings.model}</p>
				<button onClick={() => commands.chooseSettings({ model: "pending-model", reasoningEffort: "low", fastMode: true })}>Choose next model</button>
				<AgentSettingsSubmission pending={commands.choosingSettings} error={commands.settingsChoiceError} draft={commands.settingsDraft} onRetry={commands.retrySettingsChoice} onDismiss={commands.dismissSettingsChoice} />
			</>;
		}
		render(<QueryClientProvider client={client}><Choice /></QueryClientProvider>);
		const user = userEvent.setup();
		await user.click(screen.getByRole("button", { name: "Choose next model" }));
		await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
		expect(screen.getByTestId("agent-settings-submission")).toHaveTextContent("Submitting settings; not accepted yet");
		expect(screen.getByTestId("agent-settings-submission")).not.toHaveTextContent("pending-model");
		expect(screen.getByTestId("confirmed-model")).toHaveTextContent("committed-model");
		await act(async () => { finish({ error: { message: "Wait for the managed operation", code: "AGENT_SETTINGS_CONFLICT", requestId: "unaccepted-request" } }); });
		const error = await screen.findByRole("alert");
		expect(error).toHaveTextContent("Settings choice was not accepted");
		expect(error).toHaveTextContent("pending-model");
		expect(error).toHaveTextContent("AGENT_SETTINGS_CONFLICT");
		expect(error).toHaveTextContent("unaccepted-request");
		if (action === "dismiss") {
			await user.click(screen.getByRole("button", { name: "Keep current settings" }));
			expect(screen.queryByRole("alert")).not.toBeInTheDocument();
			expect(patch).toHaveBeenCalledTimes(1);
		} else {
			act(() => { client.setQueryData(key, { ...initial, pages: [{ ...initial.pages[0], settings: { model: "committed-model", reasoningEffort: "high", fastMode: false } }] }); });
			await user.click(screen.getByRole("button", { name: "Retry sending" }));
			await waitFor(() => expect(patch).toHaveBeenCalledTimes(2));
			expect(patch.mock.calls[1][1].body).toEqual({ model: "pending-model", reasoningEffort: "high", fastMode: false });
			await waitFor(() => expect(screen.getByTestId("confirmed-model")).toHaveTextContent("pending-model"));
		}
		client.clear();
	});
	it("does not show a picker selection as accepted while its PATCH is still pending", async () => {
		let finish!: (value: unknown) => void;
		patch.mockReset().mockReturnValue(new Promise((resolve) => { finish = resolve; }));
		const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
		const key = conversationQueryKey("session-a");
		const initial = { pages: [{ ...chatFixtureEmpty, sessionId: "session-a", settings: { model: "committed-model", reasoningEffort: "low" } }], pageParams: [undefined] };
		client.setQueryData(key, initial);
		function Picker() {
			const query = useQuery<InfiniteData<ConversationSnapshot>>({ queryKey: key, queryFn: () => initial, enabled: false });
			const commands = useConversationCommands("session-a");
			return <TurnSettingsBar settings={query.data?.pages[0].settings ?? {}} models={[
				{ id: "committed-model", displayName: "Committed model", default: true, efforts: ["low", "high"] },
				{ id: "pending-model", displayName: "Next model", default: false, efforts: ["low", "high"] },
			]} onChange={commands.chooseSettings} configPending={commands.choosingSettings} />;
		}
		render(<QueryClientProvider client={client}><Picker /></QueryClientProvider>);
		const user = userEvent.setup();
		const trigger = screen.getByRole("button", { name: "Model and reasoning effort for the next turn" });
		await user.click(trigger);
		await user.keyboard("{ArrowDown}{ArrowRight}");
		await user.click(await screen.findByRole("menuitemradio", { name: "Next model" }));
		await waitFor(() => expect(patch).toHaveBeenCalledTimes(1));
		expect(trigger).toHaveTextContent("Committed model");
		expect(client.getQueryData(key)).toMatchObject({ pages: [{ settings: { model: "committed-model" } }] });
		await act(async () => { finish({ data: { model: "pending-model" } }); });
		await waitFor(() => expect(trigger).toHaveTextContent("Next model"));
		client.clear();
	});
	it("updates the confirmed policy before enabling controls, without waiting on a slow snapshot refresh", async () => {
		let finish!: (value: unknown) => void;
		patch.mockReturnValue(new Promise((resolve) => { finish = resolve; }));
		const client = new QueryClient();
		const snapshot = { ...chatFixtureEmpty, sessionId: "session-a", settings: { approvalMode: "default" } };
		client.setQueryData(conversationQueryKey("session-a"), { pages: [snapshot], pageParams: [undefined] });
		vi.spyOn(client, "invalidateQueries").mockReturnValue(new Promise(() => {}));
		const wrapper = ({ children }: { children: ReactNode }) =>
			<QueryClientProvider client={client}>{children}</QueryClientProvider>;
		const { result, rerender } = renderHook(({ id }) => useConversationCommands(id), { wrapper, initialProps: { id: "session-a" } });
		act(() => result.current.chooseSettings({ approvalMode: "auto" }));
		await waitFor(() => expect(result.current.choosingSettings).toBe(true));
		// Navigation during the write must not update the newly opened session.
		rerender({ id: "session-b" });
		expect(result.current.choosingSettings).toBe(false);
		rerender({ id: "session-a" });
		expect(result.current.choosingSettings).toBe(true);
		rerender({ id: "session-b" });
		await act(async () => { finish({ data: { approvalMode: "auto" } }); });
		await waitFor(() => expect(result.current.choosingSettings).toBe(false));
		expect(client.getQueryData(conversationQueryKey("session-a"))).toMatchObject({
			pages: [{ settings: { approvalMode: "auto" } }],
		});
		expect(client.getQueryData(conversationQueryKey("session-b"))).toBeUndefined();
	});
});
