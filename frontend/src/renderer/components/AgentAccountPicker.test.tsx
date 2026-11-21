import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { AgentAccountPicker } from "./AgentAccountPicker";
import { agentAccountsQueryKey } from "../hooks/useAgentAccounts";
import { appI18n } from "../i18n";
const catalog = vi.hoisted(() => ({ scopes: [] as string[] }));
vi.mock("../hooks/useAgentModelsQuery", () => ({
	agentModelsQueryOptions: (_harness: string, scope: string) => {
		catalog.scopes.push(scope);
		return {
			queryKey: ["picker-catalog", scope],
			queryFn: async () =>
				scope.includes("projectId=proj-1")
					? { accountBinding: { accountId: "saved-role", provider: "codex", source: "role" } }
					: { accountBinding: { accountId: "g", provider: "codex", source: "global" } },
		};
	},
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
it("previews the global default for a cleared role reference, never the saved role account", async () => {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
	const account = { provider: "codex", device: false, signedIn: true, default: false };
	client.setQueryData(agentAccountsQueryKey, [
		{ ...account, id: "g", default: true, email: "global@example.test" },
		{ ...account, id: "saved-role", email: "role@example.test" },
	]);
	catalog.scopes.length = 0;
	render(
		<QueryClientProvider client={client}>
			<AgentAccountPicker harness="codex" value="" projectId="proj-1" role="worker" inheritGlobal onChange={vi.fn()} />
		</QueryClientProvider>,
	);
	const trigger = screen.getByRole("button", { name: appI18n.t("settings.agentAccounts.sessionAccount") });
	await screen.findByText(
		appI18n.t("settings.agentAccounts.inheritAccount", {
			name: "global@example.test",
			source: appI18n.t("settings.agentAccounts.source.global"),
		}),
	);
	expect(trigger).not.toHaveTextContent("role@example.test");
	expect(catalog.scopes.every((scope) => !scope.includes("proj-1"))).toBe(true);
});
