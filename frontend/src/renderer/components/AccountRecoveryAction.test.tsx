import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { AccountRecoveryAction } from "./AccountRecoveryAction";
const post = vi.hoisted(() => vi.fn());
vi.mock("../lib/api-client", () => ({
	apiClient: { POST: post },
	apiErrorMessage: () => "Disconnected",
}));
it("requires confirmation and keeps the retry dialog open until shutdown is proven", async () => {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	post
		.mockResolvedValueOnce({ data: { confirmed: false } })
		.mockResolvedValueOnce({ data: { confirmed: true } });
	render(
		<QueryClientProvider client={client}>
			<AccountRecoveryAction sessionId="fake-session" />
		</QueryClientProvider>,
	);
	const trigger = screen.getByRole("button", { name: "Stop and retry recovery" });
	await userEvent.click(trigger);
	expect(post).not.toHaveBeenCalled();
	await userEvent.keyboard("{Escape}");
	expect(trigger).toHaveFocus();
	await userEvent.click(trigger);
	await userEvent.click(
		within(screen.getByRole("dialog")).getByRole("button", { name: "Stop and retry" }),
	);
	expect(await screen.findByText(/Recovery is still unconfirmed/)).toBeVisible();
	expect(screen.getByRole("dialog")).toBeVisible();
	await userEvent.click(
		within(screen.getByRole("dialog")).getByRole("button", { name: "Stop and retry" }),
	);
	await vi.waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
	expect(post).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/account-recovery", {
		params: { path: { sessionId: "fake-session" } },
	});
});
