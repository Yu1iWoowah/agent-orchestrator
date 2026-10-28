import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { appI18n } from "../../i18n";
import { TooltipProvider } from "../ui/tooltip";
import { AgentAccountsPanel, type CliLoginControls } from "./AgentAccountsSection";

const { getMock, postMock, terminalState } = vi.hoisted(() => ({
	getMock: vi.fn(), postMock: vi.fn(),
	terminalState: { value: undefined as ((state: "attached" | "exited" | "error") => void) | undefined },
}));

vi.mock("../../lib/api-client", () => ({
	apiClient: { DELETE: vi.fn(), GET: getMock, POST: postMock },
	apiErrorCode: (error: unknown) => typeof error === "object" && error !== null && "code" in error ? error.code : undefined,
	apiErrorMessage: (error: unknown) => error instanceof Error ? error.message : "request failed",
}));

vi.mock("../TerminalPane", () => ({
	TerminalPane: ({ onTerminalStateChange }: { onTerminalStateChange?: (state: "attached" | "exited" | "error") => void }) => {
		terminalState.value = onTerminalStateChange;
		return <div data-testid="inline-terminal-body" />;
	},
}));

const device = { id: "device-claude", provider: "claude", device: true, default: true, signedIn: true, email: "a@example.com" };
const saved = { id: "acct-b", provider: "claude", device: false, default: false, signedIn: false, email: "b@example.com" };
const login = { operationId: "login-1", provider: "claude", shellTerminal: { handleId: "shellterm-1", title: "Sign in to Claude account", createdAt: "2026-10-18T10:00:00Z" } };

function renderPanel(cliLogin?: CliLoginControls) {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(<QueryClientProvider client={queryClient}><TooltipProvider><AgentAccountsPanel provider="claude" cliLogin={cliLogin} /></TooltipProvider></QueryClientProvider>);
}

const findCliLoginRow = () => waitFor(() => {
	const row = document.querySelector<HTMLElement>('[data-agent-account="device-claude"]');
	if (!row) throw new Error("no CLI login row");
	return row;
});

beforeEach(() => {
	getMock.mockReset().mockImplementation((path: string) => Promise.resolve({
		data: path === "/api/v1/agents/accounts/usage" ? { days: 30, usage: [] } : { accounts: [device, saved] },
	}));
	postMock.mockReset().mockImplementation((path: string) => {
		if (path === "/api/v1/agents/accounts/{accountId}/login") return Promise.resolve({ data: login });
		if (path === "/api/v1/agents/accounts/login/{operationId}/verify") return Promise.resolve({ data: { status: "mismatch", account: saved } });
		return Promise.resolve({ data: {} });
	});
});

it("signs a signed-out saved account in again in its own folder", async () => {
	const user = userEvent.setup();
	renderPanel();
	await user.click(await screen.findByRole("button", { name: "Sign in again" }));

	expect(postMock).toHaveBeenCalledWith("/api/v1/agents/accounts/{accountId}/login", { params: { path: { accountId: "acct-b" } } });
	expect(await screen.findByTestId("agent-account-login-terminal")).toBeInTheDocument();
	// Only the signed-out saved account offers it; it cannot be made default while signed out.
	expect(screen.getAllByRole("button", { name: "Sign in again" })).toHaveLength(1);
	expect(screen.queryByRole("button", { name: "Make default" })).not.toBeInTheDocument();

	await user.click(screen.getByRole("button", { name: "Check sign-in" }));
	await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("This account is b@example.com. Sign in again with that login."));
});

it("does not check a closed sign-in, and the remove dialog starts without an old error", async () => {
	const user = userEvent.setup();
	renderPanel();
	await user.click(await screen.findByRole("button", { name: "Sign in again" }));
	const exited = terminalState.value;
	await user.click(await screen.findByRole("button", { name: "Close sign-in" }));
	// The closed terminal still reports its exit.
	exited?.("exited");
	await user.click(screen.getAllByRole("button", { name: "Remove account" })[0]);

	expect(postMock).not.toHaveBeenCalledWith("/api/v1/agents/accounts/login/{operationId}/verify", expect.anything());
	expect(await screen.findByRole("dialog")).not.toHaveTextContent("sign-in");
});

it("checks a sign-in once when the CLI exits during a check", async () => {
	const user = userEvent.setup();
	let finish: (value: unknown) => void = () => undefined;
	postMock.mockImplementation((path: string) => {
		if (path === "/api/v1/agents/accounts/{accountId}/login") return Promise.resolve({ data: login });
		if (path === "/api/v1/agents/accounts/login/{operationId}/verify") return new Promise((resolve) => { finish = resolve; });
		return Promise.resolve({ data: {} });
	});
	renderPanel();
	await user.click(await screen.findByRole("button", { name: "Sign in again" }));
	await user.click(await screen.findByRole("button", { name: "Check sign-in" }));
	// The CLI exits while that check is still running.
	terminalState.value?.("exited");
	finish({ data: { status: "complete", account: { ...saved, signedIn: true } } });

	await waitFor(() => expect(screen.queryByTestId("agent-account-login-terminal")).not.toBeInTheDocument());
	expect(postMock.mock.calls.filter(([path]) => path === "/api/v1/agents/accounts/login/{operationId}/verify")).toHaveLength(1);
	expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

// The CLI's own login has no card of its own: its row signs it in again and
// out through the harness's login terminal, and has no folder to remove.
it("signs the CLI login in again and out from its own row", async () => {
	const user = userEvent.setup();
	const cliLogin = { disabled: false, canLogout: true, onSignIn: vi.fn(), onLogout: vi.fn() };
	renderPanel(cliLogin);
	const row = await findCliLoginRow();
	await user.click(within(row).getByRole("button", { name: "Sign in again" }));
	await user.click(within(row).getByRole("button", { name: "Log out" }));

	expect(cliLogin.onSignIn).toHaveBeenCalledTimes(1);
	expect(cliLogin.onLogout).toHaveBeenCalledTimes(1);
	expect(within(row).queryByRole("button", { name: "Remove account" })).not.toBeInTheDocument();
	expect(postMock).not.toHaveBeenCalledWith("/api/v1/agents/accounts/{accountId}/login", expect.anything());
});

it("offers only a sign-in on a signed-out CLI login, held while a login terminal is open", async () => {
	getMock.mockImplementation((path: string) => Promise.resolve({
		data: path === "/api/v1/agents/accounts/usage" ? { days: 30, usage: [] } : { accounts: [{ ...device, signedIn: false, default: false }, { ...saved, signedIn: true, default: true }] },
	}));
	renderPanel({ disabled: true, canLogout: true, onSignIn: vi.fn(), onLogout: vi.fn() });
	const row = await findCliLoginRow();
	expect(within(row).getByRole("button", { name: "Sign in again" })).toBeDisabled();
	expect(within(row).queryByRole("button", { name: "Log out" })).not.toBeInTheDocument();
});

afterEach(async () => {
	await act(async () => { await appI18n.changeLanguage("en"); });
});

it.each([
	["en", "AGENT_ACCOUNT_STORAGE_UNSAFE", "Account storage is unsafe. Check folder permissions and symbolic links, then retry."],
	["zh-TW", "AGENT_ACCOUNT_STORAGE_UNSAFE", "帳號儲存空間不安全。請檢查資料夾權限與符號連結後重試。"],
	["en", "AGENT_ACCOUNT_STORAGE_UNAVAILABLE", "Account storage is temporarily unavailable. Retry later."],
	["zh-TW", "AGENT_ACCOUNT_STORAGE_UNAVAILABLE", "帳號儲存空間暫時無法使用。請稍後重試。"],
	["en", "AGENT_ACCOUNT_STORAGE_UNSUPPORTED", "Private account profiles are currently supported on Linux only."],
	["zh-TW", "AGENT_ACCOUNT_STORAGE_UNSUPPORTED", "私有帳號 profile 目前僅支援 Linux。"],
])("shows the %s storage diagnostic for %s", async (locale, code, message) => {
	await act(async () => { await appI18n.changeLanguage(locale); });
	postMock.mockResolvedValue({ error: { code, message: "raw provider error" } });
	const user = userEvent.setup();
	renderPanel();
	await user.click(await screen.findByRole("button", { name: appI18n.t("settings.agentAccounts.signInAgain") }));
	expect(await screen.findByRole("alert")).toHaveTextContent(message);
	expect(screen.getByRole("alert")).not.toHaveTextContent("raw provider error");
});
