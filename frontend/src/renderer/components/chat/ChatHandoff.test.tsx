import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ChatComposer } from "./ChatComposer";
import { TooltipProvider } from "../ui/tooltip";
import { lexicalEditorText, typeInLexicalEditor } from "../../test/lexical";
import { readChatSessionDraft, writeChatExcerptReferences } from "../../lib/chat-drafts";

async function typeCommand(field: HTMLElement) {
	await act(async () => { await typeInLexicalEditor(field, "/handoff"); });
}

describe("AO native handoff command", () => {
	it("accepts a handoff during a turn without sending or steering provider text", async () => {
		const onHandoff = vi.fn().mockResolvedValue(undefined), onSend = vi.fn(), onSteer = vi.fn();
		render(<TooltipProvider><ChatComposer onHandoff={onHandoff} onSend={onSend} onSteer={onSteer} canSteer willQueue /></TooltipProvider>);
		const field = screen.getByLabelText("Message the agent");
		await typeCommand(field);
		fireEvent.keyDown(field, { key: "Enter", ctrlKey: true });
		await waitFor(() => expect(onHandoff).toHaveBeenCalledTimes(1));
		expect(onSend).not.toHaveBeenCalled();
		expect(onSteer).not.toHaveBeenCalled();
		await waitFor(() => expect(lexicalEditorText(field)).toBe(""));
	});

	it("keeps the command draft when acceptance cannot be confirmed", async () => {
		const onHandoff = vi.fn().mockRejectedValue(new Error("handoff backend unavailable")), onSend = vi.fn();
		render(<TooltipProvider><ChatComposer onHandoff={onHandoff} onSend={onSend} /></TooltipProvider>);
		const field = screen.getByLabelText("Message the agent");
		await typeCommand(field);
		const draftBefore = lexicalEditorText(field);
		fireEvent.keyDown(field, { key: "Enter" });
		await waitFor(() => expect(screen.getByText("handoff backend unavailable")).toBeInTheDocument());
		expect(lexicalEditorText(field)).toBe(draftBefore);
		expect(onSend).not.toHaveBeenCalled();
	});

	it("preserves attached context instead of accepting or silently dropping it", async () => {
		const id = "handoff-with-context";
		writeChatExcerptReferences(id, [{ id: "selected-context", conversationId: "source-conversation", messageId: "source-message", revision: 1, text: "keep this reference", role: "assistant" }]);
		const onHandoff = vi.fn(), onSend = vi.fn();
		render(<TooltipProvider><ChatComposer draftSessionId={id} onHandoff={onHandoff} onSend={onSend} /></TooltipProvider>);
		const field = screen.getByLabelText("Message the agent");
		await typeCommand(field);
		const draftBefore = lexicalEditorText(field);
		fireEvent.keyDown(field, { key: "Enter" });
		await waitFor(() => expect(screen.getByText("Send or remove attachments and references before requesting a handoff.")).toBeInTheDocument());
		expect(onHandoff).not.toHaveBeenCalled();
		expect(onSend).not.toHaveBeenCalled();
		expect(lexicalEditorText(field)).toBe(draftBefore);
		expect(readChatSessionDraft(id).composer.excerpts).toHaveLength(1);
	});

	it("keeps a queued edit literal instead of starting an operation", async () => {
		const onHandoff = vi.fn(), onSend = vi.fn();
		render(<TooltipProvider><ChatComposer onHandoff={onHandoff} onSend={onSend} editingQueuedTurnId="queued-handoff-text" draftSeed={{ id: "queued-handoff-text", text: "/handoff" }} /></TooltipProvider>);
		fireEvent.keyDown(screen.getByLabelText("Message the agent"), { key: "Enter" });
		await waitFor(() => expect(onSend).toHaveBeenCalledWith("/handoff", undefined, undefined, []));
		expect(onHandoff).not.toHaveBeenCalled();
	});
});
