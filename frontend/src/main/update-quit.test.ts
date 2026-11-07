// @vitest-environment node
import { afterEach, expect, it, vi } from "vitest";
import { finishUpdateQuit } from "./update-quit";
afterEach(() => vi.useRealTimers());
const actions = () => ({ quit: vi.fn(), exit: vi.fn(), log: vi.fn() });
it("allows cleanup to finish before a normal update quit", async () => {
	const a = actions();
	await finishUpdateQuit(Promise.resolve(), a);
	expect(a.quit).toHaveBeenCalledOnce();
	expect(a.exit).not.toHaveBeenCalled();
});
it("releases a hung cleanup and ignores its late completion", async () => {
	vi.useFakeTimers();
	let complete!: () => void;
	const cleanup = new Promise<void>((resolve) => { complete = resolve; });
	const a = actions();
	const done = finishUpdateQuit(cleanup, a);
	await vi.advanceTimersByTimeAsync(14_999);
	expect(a.exit).not.toHaveBeenCalled();
	await vi.advanceTimersByTimeAsync(1);
	await done;
	expect(a.exit).toHaveBeenCalledOnce();
	complete();
	await Promise.resolve();
	expect(a.quit).not.toHaveBeenCalled();
});
it("does not let a rejected cleanup prevent the prepared restart", async () => {
	const a = actions();
	const error = new Error("telemetry shutdown failed");
	await finishUpdateQuit(Promise.reject(error), a);
	expect(a.log).toHaveBeenCalledWith(error);
	expect(a.quit).toHaveBeenCalledOnce();
	expect(a.exit).not.toHaveBeenCalled();
});


it.each([false, true])("cancels the timer and ignores late cleanup (rejected=%s)", async (rejected) => {
	vi.useFakeTimers();
	const attempt = new AbortController();
	let finish = () => {};
	const cleanup = new Promise<void>((resolve, reject) => {
		finish = () => rejected ? reject(new Error("late cleanup failure")) : resolve();
	});
	const a = actions();
	const done = finishUpdateQuit(cleanup, { ...a, signal: attempt.signal });
	attempt.abort();
	await done;
	expect(vi.getTimerCount()).toBe(0);
	finish();
	await vi.advanceTimersByTimeAsync(15_000);
	expect(a.quit).not.toHaveBeenCalled();
	expect(a.exit).not.toHaveBeenCalled();
	expect(a.log).not.toHaveBeenCalled();
});

it("gives the next attempt its own full deadline", async () => {
	vi.useFakeTimers();
	const oldAttempt = new AbortController();
	const old = actions();
	const cleanup = new Promise<void>(() => {});
	const canceled = finishUpdateQuit(cleanup, { ...old, signal: oldAttempt.signal });
	await vi.advanceTimersByTimeAsync(5_000);
	oldAttempt.abort();
	await canceled;
	const next = actions();
	const done = finishUpdateQuit(cleanup, { ...next, signal: new AbortController().signal });
	await vi.advanceTimersByTimeAsync(10_000);
	expect(old.exit).not.toHaveBeenCalled();
	expect(next.exit).not.toHaveBeenCalled();
	await vi.advanceTimersByTimeAsync(5_000);
	await done;
	expect(next.exit).toHaveBeenCalledOnce();
});

it.each([false, true])("does not arm an already canceled attempt (rejected=%s)", async (rejected) => {
	vi.useFakeTimers();
	const attempt = new AbortController();
	attempt.abort();
	const a = actions();
	await finishUpdateQuit(rejected ? Promise.reject(new Error("late rejection")) : new Promise<void>(() => {}), { ...a, signal: attempt.signal });
	expect(vi.getTimerCount()).toBe(0);
	expect(a.exit).not.toHaveBeenCalled();
});
