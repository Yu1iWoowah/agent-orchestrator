import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { AgentAccountPicker } from "./AgentAccountPicker";
import { agentAccountsQueryKey } from "../hooks/useAgentAccounts";
import { appI18n } from "../i18n";
vi.mock("../hooks/useAgentModelsQuery", () => ({
	agentModelsQueryOptions: () => ({ queryKey: ["picker-catalog"], queryFn: async () => ({}) }),
}));
it("keeps an explicit selection when the default changes or the account disappears", async () => {
	const client = new QueryClient({
		defaultOptions: { queries: { retry: false, staleTime: Infinity } },
	});
	const a = {
		id: "a",
		provider: "codex",
		device: false,
		signedIn: true,
		default: true,
		email: "a@example.test",
	};
	const b = { ...a, id: "b", default: false, email: "b@example.test" };
	client.setQueryData(agentAccountsQueryKey, [a, b]);
	const onChange = vi.fn();
	const view = render(
		<QueryClientProvider client={client}>
			<AgentAccountPicker harness="codex" value="b" onChange={onChange} />
		</QueryClientProvider>,
	);
	expect(
		screen.getByRole("button", { name: appI18n.t("settings.agentAccounts.sessionAccount") }),
	).toHaveTextContent("b@example.test");
	client.setQueryData(agentAccountsQueryKey, [
		{ ...a, default: false },
		{ ...b, default: true },
	]);
	view.rerender(
		<QueryClientProvider client={client}>
			<AgentAccountPicker harness="codex" value="b" onChange={onChange} />
		</QueryClientProvider>,
	);
	expect(onChange).not.toHaveBeenCalled();
	client.setQueryData(agentAccountsQueryKey, [a]);
	await screen.findByText(appI18n.t("settings.agentAccounts.removedAccount"));
	expect(onChange).not.toHaveBeenCalled();
});
it("Escape closes the shared menu and returns focus without choosing another identity", async () => {
	const client = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity } } });
	client.setQueryData(agentAccountsQueryKey, [
		{
			id: "a",
			provider: "codex",
			device: false,
			signedIn: true,
			default: true,
			email: "fake@example.test",
		},
	]);
	const onChange = vi.fn();
	render(
		<QueryClientProvider client={client}>
			<AgentAccountPicker harness="codex" value="" onChange={onChange} />
		</QueryClientProvider>,
	);
	const trigger = screen.getByRole("button", {
		name: appI18n.t("settings.agentAccounts.sessionAccount"),
	});
	await userEvent.click(trigger);
	await userEvent.keyboard("{Escape}");
	expect(trigger).toHaveFocus();
	expect(onChange).not.toHaveBeenCalled();
});
