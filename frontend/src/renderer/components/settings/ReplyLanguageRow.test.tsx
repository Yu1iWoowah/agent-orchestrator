import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ReplyLanguageRow } from "./ReplyLanguageRow";

const mocks = vi.hoisted(() => ({
	settings: { replyLanguage: "" }, isLoading: false, error: undefined as string | undefined,
	saving: false, saveError: undefined as string | undefined, update: vi.fn(),
}));
vi.mock("../../hooks/useSettings", () => ({
	useSettings: () => ({ settings: mocks.settings, isLoading: mocks.isLoading, error: mocks.error }),
	useUpdateReplyLanguage: () => ({ update: mocks.update, saving: mocks.saving, error: mocks.saveError }),
}));

beforeEach(() => {
	mocks.settings.replyLanguage = "";
	mocks.isLoading = false;
	mocks.saving = false;
	mocks.error = undefined;
	mocks.saveError = undefined;
	mocks.update.mockReset();
});

describe("ReplyLanguageRow", () => {
	it("selects Traditional Chinese independently of the UI language", async () => {
		const user = userEvent.setup();
		render(<ReplyLanguageRow />);
		expect(screen.getByText(/new agents and agents you resume or restart/i)).toBeInTheDocument();
		await user.click(screen.getByRole("button", { name: "Agent reply language" }));
		await user.click(screen.getByRole("menuitem", { name: "繁體中文" }));
		expect(mocks.update).toHaveBeenCalledWith("zh-TW");
	});
	it("can turn injection off", async () => {
		mocks.settings.replyLanguage = "zh-TW";
		const user = userEvent.setup();
		render(<ReplyLanguageRow />);
		await user.click(screen.getByRole("button", { name: "Agent reply language" }));
		await user.click(screen.getByRole("menuitem", { name: "Off" }));
		expect(mocks.update).toHaveBeenCalledWith("");
	});
	it.each(["loading", "saving", "load error"])('disables changes during %s', (state) => {
		mocks.isLoading = state === "loading";
		mocks.saving = state === "saving";
		mocks.error = state === "load error" ? "Unable to load" : undefined;
		render(<ReplyLanguageRow />);
		expect(screen.getByRole("button", { name: "Agent reply language" })).toBeDisabled();
	});
	it("shows save failure without replacing the saved choice", () => {
		mocks.settings.replyLanguage = "zh-TW";
		mocks.saveError = "Unable to save";
		render(<ReplyLanguageRow />);
		expect(screen.getByRole("alert")).toHaveTextContent("Unable to save");
		expect(screen.getByRole("button", { name: "Agent reply language" })).toHaveTextContent("繁體中文");
	});
});
