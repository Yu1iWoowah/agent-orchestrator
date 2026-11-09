/** Squirrel has already persisted install + relaunch before this is called. */
export function finishUpdateQuit(
	cleanup: Promise<unknown>,
	actions: { quit(): void; exit(): void; log(error: unknown): void; signal?: AbortSignal },
	timeoutMs = 15_000,
): Promise<void> {
	return new Promise((resolve) => {
		const signal = actions.signal;
		if (signal?.aborted) { void cleanup.catch(() => undefined); resolve(); return; }
		let finished = false;
		const cancel = () => {
			if (finished) return;
			finished = true;
			clearTimeout(timer);
			signal?.removeEventListener("abort", cancel);
			resolve();
		};
		const finish = (forced: boolean, error?: unknown) => {
			if (finished) return;
			finished = true;
			clearTimeout(timer);
			signal?.removeEventListener("abort", cancel);
			if (error) actions.log(error);
			if (forced) actions.exit();
			else actions.quit();
			resolve();
		};
		const timer = setTimeout(() => finish(true, new Error("Update shutdown cleanup exceeded 15 seconds; exiting AO so the prepared installer can continue.")), timeoutMs);
		signal?.addEventListener("abort", cancel, { once: true });
		cleanup.then(() => finish(false), (error) => finish(false, error));
	});
}
