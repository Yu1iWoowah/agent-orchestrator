import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { AgentAccountsPage } from "./AgentAccountsPage";
import { appI18n } from "../../i18n";
import { TooltipProvider } from "../ui/tooltip";
const { getMock, postMock, deleteMock } = vi.hoisted(() => ({
	getMock: vi.fn(),
	postMock: vi.fn(),
	deleteMock: vi.fn(),
}));
vi.mock("../../lib/api-client", () => ({
	apiClient: { GET: getMock, POST: postMock, DELETE: deleteMock },
	apiErrorCode: (e: { code?: string }) => e?.code,
	apiErrorMessage: () => "Request failed",
	getApiBaseUrl: () => "http://127.0.0.1:1",
	subscribeApiBaseUrl: () => () => {},
}));
vi.mock("../../lib/navigate-to-session", () => ({ useNavigateToSession: () => vi.fn() }));
vi.mock("../TerminalPane", () => ({
	TerminalPane: () => <div data-testid="fake-login-terminal" />,
}));
const capability = (state = "supported") => ({
	state,
	reasonCode: state,
	reason: state === "supported" ? "Available." : "Unavailable.",
});
const authentication = {
	state: "authorized",
	freshness: "fresh",
	checkedAt: "2026-08-31T10:00:00Z",
	attemptedAt: "2026-08-31T10:00:00Z",
	reasonCode: "authorized",
	reason: "Codex is signed in.",
};
const capacity = {
	state: "available",
	freshness: "fresh",
	plan: "pro",
	usedPercent: 4,
	remainingPercent: 96,
	resetsAt: null,
	observedAt: "2026-08-31T10:00:00Z",
	checkedAt: "2026-08-31T10:00:00Z",
	attemptedAt: "2026-08-31T10:00:00Z",
	reasonCode: "capacity_available",
	reason: "Capacity is available.",
	overall: null,
	additionalBuckets: [],
};
const activeAccount = {
	id: "11111111-1111-4111-8111-111111111111",
	label: "active@example.com",
	source: "managed",
	status: "valid",
	reasonCode: "account_valid",
	reason: "Available.",
	active: true,
	authentication,
	authMethod: "chatgpt",
	accountEmail: "active@example.com",
	capacity,
	createdAt: "2026-08-31T09:00:00Z",
};
const inactiveAccount = {
	...activeAccount,
	id: "22222222-2222-4222-8222-222222222222",
	label: "other@example.com",
	accountEmail: "other@example.com",
	active: false,
	createdAt: "2026-08-31T09:05:00Z",
};
const accountResponse = {
	activeAccountId: activeAccount.id,
	accountRevision: 3,
	accounts: [activeAccount, inactiveAccount],
	capabilities: {
		nativeLogin: capability(),
		resetCreditConsume: capability(),
		globalSwitch: capability(),
	},
	deviceReconciliation: {
		status: "verified",
		activeAccountVerified: true,
		reasonCode: "verified",
		retryable: false,
	},
};

it("lets a legacy-conflicted managed sign-in reach logout/delete and then retry successfully", async () => {
	let legacy = [inactiveAccount];
	let managed: Array<{
		id: string;
		provider: "codex";
		signedIn: boolean;
		device: boolean;
		default: boolean;
		email: string;
	}> = [];
	const snapshot = () => ({ ...accountResponse, activeAccountId: undefined, accounts: legacy });
	getMock.mockImplementation(async (path: string) => ({
		data:
			path === "/api/v1/agents/codex/accounts"
				? snapshot()
				: path === "/api/v1/agents/accounts/usage"
					? { days: 30, usage: [] }
					: path === "/api/v1/agents/accounts"
						? { accounts: managed }
						: {},
	}));
	postMock.mockImplementation(async (path: string) => {
		if (path === "/api/v1/agents/accounts/login")
			return legacy.length
				? { error: { code: "AGENT_ACCOUNT_LEGACY_IDENTITY_CONFLICT" } }
				: {
						data: {
							operationId: "legacy-fixed",
							provider: "codex",
							shellTerminal: {
								handleId: "fake-login",
								title: "Fake sign-in",
								createdAt: "2026-10-30T00:00:00Z",
							},
						},
					};
		if (path === "/api/v1/agents/codex/accounts/{accountId}/logout") {
			legacy = legacy.map((a) => ({
				...a,
				status: "signed_out",
				authentication: { ...a.authentication, state: "unauthorized", reasonCode: "unauthorized" },
			}));
			return { data: snapshot() };
		}
		if (path === "/api/v1/agents/accounts/login/{operationId}/verify") {
			managed = [
				{
					id: "managed-after-legacy",
					provider: "codex",
					signedIn: true,
					device: false,
					default: false,
					email: inactiveAccount.label,
				},
			];
			return { data: { status: "complete", account: managed[0] } };
		}
		return { data: snapshot() };
	});
	deleteMock.mockImplementation(async () => {
		legacy = [];
		return { data: snapshot() };
	});
	const client = new QueryClient({
		defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
	});
	render(
		<QueryClientProvider client={client}>
			<TooltipProvider>
				<AgentAccountsPage />
			</TooltipProvider>
		</QueryClientProvider>,
	);
	const codex = screen.getByRole("heading", { name: "Codex" }).closest("section");
	if (!codex) throw new Error("Codex panel missing");
	await userEvent.click(within(codex).getByRole("button", { name: "Add account" }));
	expect(
		await screen.findByText(appI18n.t("settings.agentAccounts.legacyIdentityConflict")),
	).toBeVisible();
	expect(postMock).not.toHaveBeenCalledWith(
		"/api/v1/agents/codex/accounts/ensure",
		expect.anything(),
	);
	await userEvent.click(screen.getByRole("button", { name: "Manage older saved Codex logins" }));
	await userEvent.click(
		await screen.findByRole("button", { name: /other@example.com/, expanded: false }),
	);
	await userEvent.click(screen.getByRole("button", { name: "Log out" }));
	await userEvent.click(
		within(screen.getByRole("dialog")).getByRole("button", { name: "Log out" }),
	);
	const del = await screen.findByRole("button", { name: "Delete account" });
	await userEvent.click(del);
	expect(deleteMock).not.toHaveBeenCalled();
	await userEvent.click(
		within(screen.getByRole("dialog")).getByRole("button", { name: "Delete account" }),
	);
	await waitFor(() =>
		expect(deleteMock).toHaveBeenCalledWith("/api/v1/agents/codex/accounts/{accountId}", {
			params: { path: { accountId: inactiveAccount.id } },
		}),
	);
	await userEvent.click(within(codex).getByRole("button", { name: "Add account" }));
	await userEvent.click(await screen.findByRole("button", { name: "Check sign-in" }));
	await waitFor(() =>
		expect(
			document.querySelector('[data-agent-account="managed-after-legacy"]'),
		).toBeInTheDocument(),
	);
});
