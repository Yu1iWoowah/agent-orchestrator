// The AO Cloud control plane this app talks to. The daemon owns it (a baked
// default, overridden only by AO_CLOUD_CONTROL_PLANE_URL in its environment)
// and reports it in GET /api/v1/settings as cloudControlPlaneUrl, which is
// where the renderer reads it too. The main process sends the AO Cloud token
// only to this URL, never to a base URL the renderer names.

/** The daemon the resolver asks: its loopback base URL and a key for this attachment. */
export type ControlPlaneDaemon = { baseUrl: string; key: string };

export type ControlPlaneUrlResolver = () => Promise<string | null>;

/**
 * Resolve the configured control-plane URL from the attached daemon, once per
 * attachment (a new daemon, or the same port with a new pid, asks again). A
 * failed read is not kept, so the next request asks again; null means no
 * daemon is attached or it names no control plane.
 */
export function createControlPlaneUrlResolver(
	daemon: () => ControlPlaneDaemon | null,
	fetchImpl: typeof fetch = (input, init) => fetch(input, init),
): ControlPlaneUrlResolver {
	let cached: { key: string; url: Promise<string | null> } | undefined;
	return () => {
		const current = daemon();
		if (!current) return Promise.resolve(null);
		if (cached?.key !== current.key) {
			const entry = {
				key: current.key,
				url: (async () => {
					const response = await fetchImpl(`${current.baseUrl.replace(/\/+$/, "")}/api/v1/settings`, { signal: AbortSignal.timeout(5_000) });
					if (!response.ok) throw new Error(`settings answered ${response.status}`);
					const body = (await response.json()) as { cloudControlPlaneUrl?: unknown } | null;
					const url = body?.cloudControlPlaneUrl;
					return typeof url === "string" && url.trim() !== "" ? url.trim() : null;
				})(),
			};
			cached = entry;
			entry.url.catch(() => {
				if (cached === entry) cached = undefined;
			});
		}
		return cached.url.catch(() => null);
	};
}

/** Whether two control-plane base URLs name the same server and path. */
export function sameControlPlaneUrl(a: string, b: string): boolean {
	try {
		const left = new URL(a);
		const right = new URL(b);
		return left.origin === right.origin && left.pathname.replace(/\/+$/, "") === right.pathname.replace(/\/+$/, "");
	} catch {
		return false;
	}
}

function invalid(detail: string): Error {
	return new Error(`Invalid cloud control-plane request: ${detail}`);
}

function isLoopbackHost(hostname: string): boolean {
	return hostname === "localhost" || hostname === "127.0.0.1";
}

/** A renderer-supplied CP base URL: https, or http on loopback only. */
export function parseCloudBaseUrl(baseUrl: unknown): URL {
	if (typeof baseUrl !== "string" || baseUrl === "") throw invalid("baseUrl must be a non-empty string.");
	let base: URL;
	try {
		base = new URL(baseUrl);
	} catch {
		throw invalid(`baseUrl ${JSON.stringify(baseUrl)} is not a valid URL.`);
	}
	const httpsOk = base.protocol === "https:";
	const loopbackHttpOk = base.protocol === "http:" && isLoopbackHost(base.hostname);
	if (!httpsOk && !loopbackHttpOk) {
		throw invalid("baseUrl must use https (http is allowed only for localhost/127.0.0.1).");
	}
	if (base.username !== "" || base.password !== "" || base.search !== "" || base.hash !== "") {
		throw invalid("baseUrl must not carry credentials, a query string, or a fragment.");
	}
	return base;
}

/** Validate the renderer's destination before any credential is read. */
export async function pinControlPlaneUrl(baseUrl: string, resolve: ControlPlaneUrlResolver): Promise<string> {
	const requested = parseCloudBaseUrl(baseUrl);
	const configured = await resolve();
	if (!configured) throw invalid("no Orchestrator Cloud control plane is configured.");
	const pinned = parseCloudBaseUrl(configured);
	if (!sameControlPlaneUrl(requested.href, pinned.href)) {
		throw invalid("baseUrl is not the configured Orchestrator Cloud control plane.");
	}
	return pinned.href;
}
