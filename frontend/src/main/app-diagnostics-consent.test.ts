import { afterEach, expect, it, vi } from "vitest";
import fixture from "../../../test/fixtures/app-diagnostics-v1.json";
import { AppDiagnosticsMain } from "./app-diagnostics";
const actors: AppDiagnosticsMain[] = [];
afterEach(() => {
	for (const actor of actors.splice(0)) actor.dispose();
});
function harness(confirm = vi.fn(async () => true)) {
	const shell = { id: 1, mainFrame: { url: "app://renderer/index.html" }, isDestroyed: () => false, send: vi.fn() };
	const grant = vi.fn(async (_session: string) => "grant-1");
	const revoke = vi.fn(async (_id: string) => undefined);
	const actor = new AppDiagnosticsMain({
		instanceId: "app-1",
		rendererURL: () => shell.mainFrame.url,
		shell: () => shell,
		identity: () => fixture.identity,
		consent: { confirm, grant, revoke },
	});
	actors.push(actor);
	actor.setConnection({ connected: true });
	const event = { sender: shell, senderFrame: shell.mainFrame };
	const doc = actor.register(event);
	const request = {
		schemaVersion: 1,
		requestId: "consent-1",
		documentId: doc.documentId,
		windowId: doc.windowId,
		generation: doc.generation,
		sessionId: "next.js-1",
	};
	return { actor, event, request, grant, revoke, confirm };
}
it("requires native confirmation and returns only grant metadata", async () => {
	const h = harness();
	const result = await h.actor.requestEnable(h.event, h.request);
	expect(h.confirm).toHaveBeenCalledWith("next.js-1");
	expect(h.grant).toHaveBeenCalledWith("next.js-1");
	expect(result).toMatchObject({ outcome: "enabled", grantId: "grant-1", sessionId: "next.js-1" });
	expect(h.actor.readConsent(h.event)).toMatchObject({ grantId: "grant-1" });
	expect(JSON.stringify(result)).not.toContain("token");
	await expect(h.actor.requestEnable(h.event, h.request)).rejects.toThrow();
});
it("native cancel does not enable collector or create a grant", async () => {
	const h = harness(vi.fn(async () => false));
	expect(await h.actor.requestEnable(h.event, h.request)).toEqual({ ...h.request, outcome: "cancelled" });
	expect(h.grant).not.toHaveBeenCalled();
	expect(h.actor.windows()).toEqual([]);
});
it("rejects stale document and changed connection during native confirmation", async () => {
	let finish!: (value: boolean) => void;
	const h = harness(
		vi.fn(
			() =>
				new Promise<boolean>((resolve) => {
					finish = resolve;
				}),
		),
	);
	const pending = h.actor.requestEnable(h.event, h.request);
	h.actor.resetDocument();
	finish(true);
	await expect(pending).rejects.toThrow("APP_RENDERER_CHANGED");
	expect(h.grant).not.toHaveBeenCalled();
	await expect(h.actor.requestEnable(h.event, { ...h.request, requestId: "new" })).rejects.toThrow();
});
it.each([undefined, 2])("rejects missing/unsupported consent schema %s", async (schemaVersion) => {
	const h = harness();
	await expect(h.actor.requestEnable(h.event, { ...h.request, schemaVersion })).rejects.toThrow();
	expect(h.confirm).not.toHaveBeenCalled();
});
it("fails closed when grant creation fails and clears recording", async () => {
	const h = harness();
	h.grant.mockRejectedValueOnce(new Error("APP_DIAGNOSTICS_STORAGE_FAILED"));
	await expect(h.actor.requestEnable(h.event, h.request)).rejects.toThrow("APP_DIAGNOSTICS_STORAGE_FAILED");
	expect(h.actor.windows()).toEqual([]);
});
it("rechecks identity after grant creation and revokes a late grant", async () => {
	const h = harness();
	h.grant.mockImplementationOnce(async () => {
		h.actor.resetDocument();
		return "late-grant";
	});
	await expect(h.actor.requestEnable(h.event, h.request)).rejects.toThrow("APP_RENDERER_CHANGED");
	expect(h.revoke).toHaveBeenCalledWith("late-grant");
	expect(h.actor.windows()).toEqual([]);
});

it("revoking native consent cancels an in-flight capture before waiting for disk cleanup", async () => {
	const h = harness();
	await h.actor.requestEnable(h.event, h.request);
	const capture = h.actor.capture({}, new AbortController().signal);
	const rejected = expect(capture).rejects.toThrow("APP_DIAGNOSTICS_DISABLED");
	const { sessionId: _, ...request } = h.request;
	await h.actor.requestRevoke(h.event, { ...request, requestId: "revoke-1" });
	await rejected;
	expect(h.revoke).toHaveBeenCalledWith("grant-1");
	expect(h.actor.readConsent(h.event)).toBeNull();
	expect(h.actor.windows()).toEqual([]);
});

it("never exports native or transport error contents across the consent boundary", async () => {
	const confirm = vi.fn(async () => {
		throw new Error("CANARY_SECRET /private/account.json");
	});
	const h = harness(confirm);
	await expect(h.actor.requestEnable(h.event, h.request)).rejects.toThrow(/^APP_DIAGNOSTICS_UNSUPPORTED$/);
	expect(h.actor.windows()).toEqual([]);
});

it("clears native consent and recording when the daemon revokes the grant", async () => {
	const h = harness();
	await h.actor.requestEnable(h.event, h.request);
	h.actor.grantRevoked("other-grant");
	expect(h.actor.readConsent(h.event)).not.toBeNull();
	h.actor.grantRevoked("grant-1");
	expect(h.actor.readConsent(h.event)).toBeNull();
	expect(h.actor.windows()).toEqual([]);
});
