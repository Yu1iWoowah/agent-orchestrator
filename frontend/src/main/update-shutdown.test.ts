// @vitest-environment node
import { spawn, type ChildProcess } from "node:child_process";
import { once } from "node:events";
import { afterEach, expect, it, vi } from "vitest";
import { confirmUnsafeChatDraftLeave } from "./chat-draft-unload";
import { createUpdateShutdownLifecycle, requestUpdateShutdown, waitForUpdateDaemonExit } from "./protected-side-quit";

const children: ChildProcess[] = [];
afterEach(async () => {
	for (const child of children.splice(0)) {
		if (child.exitCode !== null || child.signalCode !== null) continue;
		const exited = once(child, "exit");
		child.kill("SIGTERM");
		await exited;
	}
});

async function daemonFixture() {
	const child = spawn(process.execPath, ["-e", `
		const server = require('node:http').createServer((req, res) => {
			if (req.url === '/shutdown') {
				res.writeHead(202).end();
				server.close();
				setTimeout(() => process.exit(0), 200);
			} else res.end('session accepted');
		});
		server.listen(0, '127.0.0.1', () => console.log(server.address().port));
	`], { stdio: ["ignore", "pipe", "inherit"] });
	children.push(child);
	if (!child.stdout) throw new Error("fixture stdout unavailable");
	const startup = new AbortController();
	const [data] = await Promise.race([
		once(child.stdout, "data", { signal: startup.signal }),
		once(child, "exit", { signal: startup.signal }).then(([code]) => { throw new Error(`fixture exited before listening: ${code}`); }),
	]).finally(() => startup.abort());
	const port = Number(String(data).trim());
	if (!Number.isInteger(port) || port <= 0) throw new Error("fixture port unavailable");
	return { child, baseUrl: `http://127.0.0.1:${port}` };
}
const copy = { title: "Draft", message: "Unsaved", detail: "Attachment pending", stay: "Stay", leave: "Leave" };
const isProcessAlive = (pid: number) => { try { process.kill(pid, 0); return true; } catch { return false; } };

it("keeps serving work when the pending-attachment draft prompt is canceled before POST /shutdown", async () => {
	const daemon = await daemonFixture();
	const shutdown = vi.fn(() => requestUpdateShutdown(daemon.baseUrl));
	const restore = vi.fn(async () => undefined);
	let approved = false;
	const lifecycle = createUpdateShutdownLifecycle({
		confirmLeave: () => confirmUnsafeChatDraftLeave(["pending-attachments"], () => 0, copy),
		requestShutdown: shutdown, restoreDaemon: restore,
		setApproved: (value) => { approved = value; },
	});
	expect(await lifecycle.prepare()).toBe(false);
	expect(await (await fetch(`${daemon.baseUrl}/work`)).text()).toBe("session accepted");
	expect(approved).toBe(false);
	expect(shutdown).not.toHaveBeenCalled();
	expect(restore).not.toHaveBeenCalled();
});

it.each([false, true])("recovers work after accepted shutdown and installer failure (adopted=%s)", async (adopted) => {
	let daemon = await daemonFixture();
	let approved = false;
	let admissions = 0;
	const lifecycle = createUpdateShutdownLifecycle({
		confirmLeave: () => confirmUnsafeChatDraftLeave(["pending-attachments"], () => 1, copy),
		requestShutdown: async () => { await requestUpdateShutdown(daemon.baseUrl); admissions++; },
		setApproved: (value) => { approved = value; },
		restoreDaemon: async () => {
			const old = daemon;
			await waitForUpdateDaemonExit({ child: adopted ? null : old.child, pid: old.child.pid, isProcessAlive });
			expect(old.child.exitCode).toBe(0);
			daemon = await daemonFixture();
		},
	});
	expect(await lifecycle.prepare()).toBe(true);
	expect(approved).toBe(true);
	// Installer failure leaves this process alive. Parallel failure notifications
	// share recovery, and a retry waits for it rather than reusing the old permit.
	const first = lifecycle.recover();
	const duplicate = lifecycle.recover();
	expect(approved).toBe(false);
	await Promise.all([first, duplicate]);
	expect(await (await fetch(`${daemon.baseUrl}/work`)).text()).toBe("session accepted");
	expect(admissions).toBe(1);
	expect(await lifecycle.prepare()).toBe(true);
	expect(admissions).toBe(2);
	await lifecycle.recover();
});
