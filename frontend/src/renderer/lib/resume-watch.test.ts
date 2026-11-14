import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
	clearResumeFailure,
	followResume,
	noteResumeFailure,
	RESUME_CHECK_LIMIT_MS,
	RESUME_WATCH_INTERVAL_MS,
	RESUME_WATCH_LIMIT_MS,
	resetResumeWatches,
	resumeFailed,
	shouldResumeOnOpen,
	watchResume,
} from "./resume-watch";

const mocks = vi.hoisted(() => ({ get: vi.fn(), show: vi.fn(), hosts: [] as Array<string | undefined> }));
vi.mock("./host-clients", () => ({
	clientForSessionHost: (hostId?: string) => {
		mocks.hosts.push(hostId);
		return { GET: mocks.get };
	},
}));
vi.mock("./bridge", () => ({ aoBridge: { notifications: { show: mocks.show } } }));

function status(state: string, extra: object = {}) {
	return { data: { sessionId: "s-1", state, ...extra }, response: { status: 200 } };
}

function failure(code: string, message = "nope") {
	return status("failed", { error: { status: 409, code, message } });
}

async function tick(times = 1) {
	for (let i = 0; i < times; i++) await vi.advanceTimersByTimeAsync(RESUME_WATCH_INTERVAL_MS);
}

beforeEach(() => {
	vi.useFakeTimers();
	resetResumeWatches();
	mocks.get.mockReset();
	mocks.show.mockReset().mockResolvedValue(undefined);
	mocks.hosts.length = 0;
});

afterEach(() => {
	vi.useRealTimers();
});

describe("watchResume", () => {
	it("records a failure and refreshes once the background resume fails", async () => {
		const refresh = vi.fn();
		mocks.get.mockResolvedValueOnce(status("running")).mockResolvedValueOnce(failure("AGENT_RESUME_FAILED", "AO could not restart the agent"));
		watchResume("s-1", undefined, refresh);
		await tick();
		expect(resumeFailed("s-1")).toBe(false);
		await tick();
		expect(resumeFailed("s-1")).toBe(true);
		expect(refresh).toHaveBeenCalledTimes(1);
		await tick(3);
		expect(mocks.get).toHaveBeenCalledTimes(2);
	});

	it("does not record a failure that met another resume in progress", async () => {
		const refresh = vi.fn();
		mocks.get.mockResolvedValue(failure("AGENT_RESUME_IN_PROGRESS"));
		watchResume("s-1", undefined, refresh);
		await tick();
		expect(resumeFailed("s-1")).toBe(false);
		expect(refresh).toHaveBeenCalledTimes(1);
	});

	it("stops quietly when the host has no record of the resume", async () => {
		const refresh = vi.fn();
		mocks.get.mockResolvedValue({ data: undefined, error: { code: "AGENT_RESUME_NOT_FOUND" }, response: { status: 404 } });
		watchResume("s-1", "box-a", refresh);
		await tick(3);
		expect(mocks.get).toHaveBeenCalledTimes(1);
		expect(mocks.hosts).toEqual(["box-a"]);
		expect(resumeFailed("s-1", "box-a")).toBe(false);
	});

	it("clears a recorded failure and announces a saved-prompt start on success", async () => {
		noteResumeFailure("s-1", { code: "AGENT_RESUME_FAILED", message: "earlier" });
		mocks.get.mockResolvedValue(status("succeeded", { resumeMode: "saved_prompt" }));
		watchResume("s-1", undefined, vi.fn());
		expect(resumeFailed("s-1")).toBe(false);
		await tick();
		expect(resumeFailed("s-1")).toBe(false);
		expect(mocks.show).toHaveBeenCalledWith(expect.objectContaining({ id: expect.stringMatching(/^resume-agent-fallback:local:s-1:/) }));
	});

	it("follows one resume per host and session and gives up after the limit", async () => {
		const refresh = vi.fn();
		mocks.get.mockResolvedValue(status("running"));
		watchResume("s-1", undefined, refresh);
		watchResume("s-1", undefined, refresh);
		await tick();
		expect(mocks.get).toHaveBeenCalledTimes(1);
		await vi.advanceTimersByTimeAsync(RESUME_WATCH_LIMIT_MS + RESUME_WATCH_INTERVAL_MS);
		const reads = mocks.get.mock.calls.length;
		await tick(3);
		expect(mocks.get).toHaveBeenCalledTimes(reads);
		expect(resumeFailed("s-1")).toBe(false);
		expect(refresh).toHaveBeenCalledTimes(1);
	});
});

describe("watchResume with a read that never answers", () => {
	it("abandons the pending read and ends the watch at the limit", async () => {
		const refresh = vi.fn();
		const signals: AbortSignal[] = [];
		mocks.get.mockImplementation((_path: string, init: { signal: AbortSignal }) => {
			signals.push(init.signal);
			return new Promise((_resolve, reject) => init.signal.addEventListener("abort", () => reject(new Error("aborted"))));
		});
		watchResume("s-1", undefined, refresh);
		await tick();
		expect(mocks.get).toHaveBeenCalledTimes(1);
		await vi.advanceTimersByTimeAsync(RESUME_WATCH_LIMIT_MS);
		expect(signals[0].aborted).toBe(true);
		expect(refresh).toHaveBeenCalledTimes(1);
		watchResume("s-1", undefined, refresh);
		await tick();
		expect(mocks.get).toHaveBeenCalledTimes(2);
	});

	it("resumes on open when the status read does not answer in time", async () => {
		mocks.get.mockImplementation((_path: string, init: { signal: AbortSignal }) =>
			new Promise((_resolve, reject) => init.signal.addEventListener("abort", () => reject(new Error("aborted")))));
		const decision = shouldResumeOnOpen("s-1", vi.fn());
		await vi.advanceTimersByTimeAsync(RESUME_CHECK_LIMIT_MS);
		await expect(decision).resolves.toBe(true);
	});
});

describe("followResume", () => {
	it("treats a 200 from an older host as finished", async () => {
		followResume("s-1", "box-a", { status: 200 }, { resumeMode: "saved_prompt" }, vi.fn());
		await tick(2);
		expect(mocks.get).not.toHaveBeenCalled();
		expect(mocks.show).toHaveBeenCalledWith(expect.objectContaining({ id: expect.stringMatching(/^resume-agent-fallback:box-a:s-1:/) }));
	});
});

describe("noteResumeFailure", () => {
	it("ignores a resume already in progress and forgets a failure once cleared", () => {
		noteResumeFailure("s-1", { code: "AGENT_RESUME_IN_PROGRESS" });
		expect(resumeFailed("s-1")).toBe(false);
		noteResumeFailure("s-1", { code: "AGENT_RESUME_FAILED" });
		expect(resumeFailed("s-1")).toBe(true);
		clearResumeFailure("s-1");
		expect(resumeFailed("s-1")).toBe(false);
	});
});

describe("shouldResumeOnOpen", () => {
	it("resumes when the daemon has no record or an in-progress failure", async () => {
		mocks.get.mockResolvedValueOnce({ data: undefined, response: { status: 404 } });
		await expect(shouldResumeOnOpen("s-1", vi.fn())).resolves.toBe(true);
		mocks.get.mockResolvedValueOnce(failure("AGENT_RESUME_IN_PROGRESS"));
		await expect(shouldResumeOnOpen("s-1", vi.fn())).resolves.toBe(true);
		expect(resumeFailed("s-1")).toBe(false);
	});

	it("keeps a recorded failure instead of resuming again", async () => {
		mocks.get.mockResolvedValueOnce(failure("AGENT_ACCOUNT_IN_USE"));
		await expect(shouldResumeOnOpen("s-1", vi.fn())).resolves.toBe(false);
		expect(resumeFailed("s-1")).toBe(true);
	});

	it("follows a running resume instead of starting another", async () => {
		mocks.get.mockResolvedValueOnce(status("running")).mockResolvedValueOnce(status("succeeded", { resumeMode: "native" }));
		const refresh = vi.fn();
		await expect(shouldResumeOnOpen("s-1", refresh)).resolves.toBe(false);
		await tick();
		expect(refresh).toHaveBeenCalledTimes(1);
	});
});
