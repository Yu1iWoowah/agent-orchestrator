import { act, renderHook } from "@testing-library/react";
import { StrictMode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { acceptOrchestratorHandoff, useOrchestratorHandoff } from "./useOrchestratorHandoff";

const mocks = vi.hoisted(() => ({ post: vi.fn() }));
vi.mock("../lib/host-clients", () => ({ clientForSessionHost: () => ({ POST: mocks.post }) }));
const source = { projectId: "handoff-project", sourceSessionId: "source-orchestrator", hostId: "local" };
const id = "11111111-1111-4111-8111-111111111111";
function accepted(phase = "accepted", target = source, operationId = id, operationKey = "unmatched-key") {
	return { data: { operation: { id: operationId, operationKey, projectId: target.projectId, sourceSessionId: target.sourceSessionId, phase } }, response: new Response(null, { status: 202 }) };
}

function acceptNext(phase = "accepted", target = source, operationId = id) {
	mocks.post.mockImplementationOnce((_path: string, request: { body: { operationKey: string } }) => Promise.resolve(accepted(phase, target, operationId, request.body.operationKey)));
}

beforeEach(() => { mocks.post.mockReset(); window.localStorage.clear(); });

describe("handoff intent and receipt", () => {
	it("rejects another operation key without replacing the pending retry identity", async () => {
		mocks.post.mockResolvedValueOnce(accepted("accepted", source, id, "22222222-2222-4222-8222-222222222222"));
		await expect(acceptOrchestratorHandoff(source, window.localStorage)).rejects.toThrow("ORCHESTRATOR_HANDOFF_RECEIPT_INVALID");
		const firstKey = mocks.post.mock.calls[0][1].body.operationKey;
		acceptNext();
		await acceptOrchestratorHandoff(source, window.localStorage);
		expect(mocks.post.mock.calls[1][1].body.operationKey).toBe(firstKey);
	});
	it("reuses the persisted operation key after a lost response and view remount", async () => {
		mocks.post.mockRejectedValueOnce(new Error("reply lost"));
		acceptNext();
		const first = renderHook(() => useOrchestratorHandoff(source));
		await act(async () => { await expect(first.result.current.request()).rejects.toThrow("reply lost"); });
		const key = mocks.post.mock.calls[0][1].body.operationKey;
		first.unmount();
		const second = renderHook(() => useOrchestratorHandoff(source));
		await act(async () => { await second.result.current.request(); });
		expect(mocks.post.mock.calls[1][1].body.operationKey).toBe(key);
		expect(mocks.post.mock.calls[1]).toEqual(["/api/v1/projects/{id}/orchestrator/replacements", { params: { path: { id: source.projectId } }, body: { sourceSessionId: source.sourceSessionId, operationKey: key } }]);
		expect(second.result.current.receipt?.id).toBe(id);
		expect(second.result.current.receipt?.readCommand).toBe(`ao orchestrator replacement get ${id} --json`);
	});

	it("replays a known accepted operation without allocating another identity", async () => {
		acceptNext(); acceptNext("recovery_required");
		const first = await acceptOrchestratorHandoff(source, window.localStorage);
		const firstKey = mocks.post.mock.calls[0][1].body.operationKey;
		expect(first.id).toBe(id);
		const view = renderHook(() => useOrchestratorHandoff(source));
		expect(view.result.current.receipt?.id).toBe(id);
		await act(async () => { await view.result.current.request(); });
		expect(mocks.post.mock.calls[1][1].body.operationKey).toBe(firstKey);
		expect(view.result.current.receipt?.phase).toBe("recovery_required");
	});

	it("clears a definitive rejection but keeps identity on an unknown server error", async () => {
		mocks.post.mockResolvedValueOnce({ error: { error: { message: "unsupported source" } }, response: new Response(null, { status: 409 }) })
			.mockResolvedValueOnce({ error: { error: { message: "unknown outcome" } }, response: new Response(null, { status: 500 }) });
		acceptNext();
		await expect(acceptOrchestratorHandoff(source, window.localStorage)).rejects.toThrow();
		await expect(acceptOrchestratorHandoff(source, window.localStorage)).rejects.toThrow();
		await acceptOrchestratorHandoff(source, window.localStorage);
		const keys = mocks.post.mock.calls.map(call => call[1].body.operationKey);
		expect(keys[0]).not.toBe(keys[1]);
		expect(keys[1]).toBe(keys[2]);
	});

	it("does not undo a cancellation observed in a reply", async () => {
		acceptNext(); acceptNext("cancelled"); acceptNext();
		await acceptOrchestratorHandoff(source, window.localStorage);
		const cancelled = await acceptOrchestratorHandoff(source, window.localStorage);
		expect(cancelled.phase).toBe("cancelled");
		expect(mocks.post).toHaveBeenCalledTimes(2);
		await acceptOrchestratorHandoff(source, window.localStorage);
		const keys = mocks.post.mock.calls.map(call => call[1].body.operationKey);
		expect(keys[0]).toBe(keys[1]);
		expect(keys[2]).not.toBe(keys[1]);
	});

	it("refuses before HTTP if it cannot retain a new request identity", async () => {
		const storage: Storage = { length: 0, getItem: () => null, key: () => null, clear: () => {}, removeItem: () => {}, setItem: () => { throw new Error("quota"); } };
		await expect(acceptOrchestratorHandoff(source, storage)).rejects.toThrow();
		expect(mocks.post).not.toHaveBeenCalled();
	});

	it("shows acceptance after StrictMode remounts its effects", async () => {
		acceptNext();
		const view = renderHook(() => useOrchestratorHandoff(source), { wrapper: StrictMode });
		await act(async () => { await view.result.current.request(); });
		expect(view.result.current.receipt?.id).toBe(id);
	});

	it("joins concurrent submission and ignores late receipts from another source", async () => {
		let resolve!: (value: ReturnType<typeof accepted>) => void;
		mocks.post.mockReturnValueOnce(new Promise<ReturnType<typeof accepted>>(done => { resolve = done; }));
		const view = renderHook(input => useOrchestratorHandoff(input), { initialProps: source });
		const first = view.result.current.request();
		expect(view.result.current.request()).toBe(first);
		expect(mocks.post).toHaveBeenCalledTimes(1);
		const next = { ...source, projectId: "other-project", sourceSessionId: "next-orchestrator" };
		const nextID = "22222222-2222-4222-8222-222222222222";
		view.rerender(next);
		acceptNext("accepted", next, nextID);
		await act(async () => { await view.result.current.request(); });
		await act(async () => { resolve(accepted("accepted", source, id, mocks.post.mock.calls[0][1].body.operationKey)); await first; });
		expect(view.result.current.receipt?.id).toBe(nextID);
	});

	it("keeps host and source identities separate and rejects mismatched receipts", async () => {
		acceptNext(); acceptNext();
		await acceptOrchestratorHandoff(source, window.localStorage);
		const other = { ...source, hostId: "another-host" };
		await acceptOrchestratorHandoff(other, window.localStorage);
		expect(mocks.post.mock.calls[0][1].body.operationKey).not.toBe(mocks.post.mock.calls[1][1].body.operationKey);
		acceptNext("accepted", { ...source, sourceSessionId: "other-source" });
		await expect(acceptOrchestratorHandoff(source, window.localStorage)).rejects.toThrow("ORCHESTRATOR_HANDOFF_RECEIPT_INVALID");
	});
});
