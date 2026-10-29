import { describe, expect, it, vi } from "vitest";
import { createDocumentResources, staticDocumentHtml } from "./document-html";
const base = {
	kind: "shared" as const,
	projectId: "p",
	path: "dir/index.html",
};
describe("opaque static documents", () => {
	it("preserves media and container range comparisons while escaping style end tags", async () => {
		const resources = createDocumentResources(base, vi.fn());
		const result = await staticDocumentHtml(
			String.raw`<style>@media (width < 600px){h1{color:red}}@container (width < 500px){p{color:blue}}.x::after{content:"\3c/STYLE\3e\3c a href=http://127.0.0.1:3001/\3e x\3c/a\3e"}</style>`,
			resources,
		);
		const parsed = new DOMParser().parseFromString(result.html, "text/html");
		const stylesheet = parsed.querySelector("style")?.textContent;
		expect(stylesheet).toContain("@media (width<600px)");
		expect(stylesheet).toContain("@container (width<500px)");
		expect(parsed.querySelector("a[href]")).toBeNull();
		resources.dispose();
	});

	it.each(["inline", "linked"])(
		"keeps %s CSS strings inside style after HTML serialization",
		async (source) => {
			const payload =
				'</style><a href="http://127.0.0.1:3001/">injected</a><div id="injected">';
			const stylesheet = `.x::after{content:${JSON.stringify(payload)}}`;
			const resources = createDocumentResources(
				base,
				async () => new TextEncoder().encode(stylesheet).buffer,
			);
			const html =
				source === "inline"
					? String.raw`<style>.x::after{content:"\3c/style\3e\3c a href=http://127.0.0.1:3001/\3e injected\3c/a\3e\3c div id=injected\3e"}</style><p>safe</p>`
					: '<link rel="stylesheet" href="main.css"><p>safe</p>';
			const result = await staticDocumentHtml(html, resources);
			const parsed = new DOMParser().parseFromString(result.html, "text/html");
			expect(parsed.querySelector("a[href],#injected")).toBeNull();
			expect([...parsed.body.children].map((node) => node.tagName)).toEqual([
				"P",
			]);
			expect(parsed.querySelector("style")?.textContent).toContain("content:");
			resources.dispose();
		},
	);

	it("removes image-map navigation even when the map's local raster can load", async () => {
		const png = new Uint8Array(24);
		png.set([137, 80, 78, 71, 13, 10, 26, 10]);
		new DataView(png.buffer).setUint32(16, 1);
		new DataView(png.buffer).setUint32(20, 1);
		const resources = createDocumentResources(
			base,
			async () => png.buffer,
			"inline-raster",
		);
		const result = await staticDocumentHtml(
			'<img usemap="#m" src="same-dir.png"><map name="m"><area shape="rect" coords="0,0,800,600" href="http://127.0.0.1:3001/"></map>',
			resources,
		);
		const parsed = new DOMParser().parseFromString(result.html, "text/html");
		expect(parsed.querySelector("area[href]")).toBeNull();
		expect(parsed.querySelector("img")?.getAttribute("src")).toMatch(
			/^data:image\/png;base64,/,
		);
		resources.dispose();
	});

	it("keeps ordinary CSS and removes only unsafe declarations and unsupported at-rules", async () => {
		const load = vi.fn().mockRejectedValue(new Error("missing"));
		const resources = createDocumentResources(base, load);
		const result = await staticDocumentHtml(
			String.raw`<style>
			:root{--accent:#06c;--unsafe:image-set("https://evil.invalid/a.png" 1x)}
			h1{color:var(--accent);filter:blur(1px);background:i\6d age-set("https://evil.invalid/b.png" 1x)}
			.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(200px,1fr));padding:env(safe-area-inset-top)}
			@keyframes spin{to{transform:rotate(360deg)}}
			@font-face{font-family:evil;src:url(https://evil.invalid/font.woff)}
			p{color:blue;background:url(https://evil.invalid/c.png)}
		</style><div style="color:var(--accent);background:image-set('https://evil.invalid/d.png' 1x);display:grid">safe</div>`,
			resources,
		);
		const parsed = new DOMParser().parseFromString(result.html, "text/html");
		const css = parsed.querySelector("style")?.textContent;
		expect(css).toContain("--accent:#06c");
		expect(css).toContain("color:var(--accent)");
		expect(css).toContain("repeat(auto-fill,minmax(200px,1fr))");
		expect(css).toContain("filter:blur(1px)");
		expect(css).toContain("env(safe-area-inset-top)");
		expect(css).toContain("color:blue");
		expect(css).not.toMatch(/image-set|keyframes|font-face|https:/);
		expect(parsed.querySelector("div")?.getAttribute("style")).toBe(
			"color:var(--accent);display:grid",
		);
		expect(load).not.toHaveBeenCalled();
		expect(parsed.querySelector("meta")?.content).toContain(
			"img-src data: blob:",
		);
		resources.dispose();
	});

	it("removes executable markup and never requests remote, escaped or app API resources", async () => {
		const load = vi.fn().mockRejectedValue(new Error("missing"));
		const resources = createDocumentResources(base, load);
		const result = await staticDocumentHtml(
			`<base href="http://localhost"><meta http-equiv="refresh" content="0;url=http://localhost"><script>top.pwn=1</script><form action="/api/v1/shutdown"><input><button>go</button></form><svg onload="top.pwn=2"></svg><img src="http://127.0.0.1/api/v1/status" onerror="top.pwn=3"><iframe src="https://evil.invalid"></iframe><style>@import "https://evil.invalid/a.css"; .x{background:url(//evil.invalid/x)} .y{background:url(../../escape.png)}</style><div style="background:url(https://evil.invalid/y)">safe</div>`,
			resources,
		);
		expect(load).not.toHaveBeenCalled();
		const parsed = new DOMParser().parseFromString(result.html, "text/html");
		expect(
			parsed.querySelector("script,base,form,iframe,svg,input,button"),
		).toBeNull();
		expect(parsed.querySelector("[onerror],[onload]")).toBeNull();
		expect(
			parsed
				.querySelector('meta[http-equiv="Content-Security-Policy"]')
				?.getAttribute("content"),
		).toContain("connect-src 'none'");
		expect(parsed.querySelector("img")?.getAttribute("src")).toBeNull();
		resources.dispose();
	});
	it("resolves nested stylesheet URLs within one root and owns and revokes every blob", async () => {
		const create = vi
			.spyOn(URL, "createObjectURL")
			.mockImplementation(() => "blob:owned-" + Math.random());
		const revoke = vi
			.spyOn(URL, "revokeObjectURL")
			.mockImplementation(() => {});
		const png = new Uint8Array(24);
		png.set([137, 80, 78, 71, 13, 10, 26, 10]);
		new DataView(png.buffer).setUint32(16, 1);
		new DataView(png.buffer).setUint32(20, 1);
		const load = vi.fn(async (target: { path: string }) =>
			target.path.endsWith(".css")
				? new TextEncoder().encode(
						'@import "nested.css";p{background:url(../pic.png)}',
					).buffer
				: png.buffer,
		);
		const resources = createDocumentResources(base, load);
		const result = await staticDocumentHtml(
			'<link rel="stylesheet" href="styles/main.css"><img src="pic.png"><a href="../guide.md#title">guide</a>',
			resources,
		);
		expect(result.html).toContain("blob:owned-");
		expect(result.links).toEqual([
			{ label: "guide", href: "../guide.md#title" },
		]);
		expect(load.mock.calls.map(([target]) => target.path)).toEqual(
			expect.arrayContaining([
				"dir/styles/main.css",
				"dir/styles/nested.css",
				"dir/pic.png",
			]),
		);
		resources.dispose();
		expect(revoke).toHaveBeenCalledTimes(create.mock.calls.length);
		create.mockRestore();
		revoke.mockRestore();
	});
});

it("shares identical image blobs and enforces per-resource, total and source-count budgets", async () => {
	const create = vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:owned");
	const revoke = vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
	const png = (size: number) => {
		const b = new Uint8Array(size);
		b.set([137, 80, 78, 71, 13, 10, 26, 10]);
		new DataView(b.buffer).setUint32(16, 1);
		new DataView(b.buffer).setUint32(20, 1);
		return b.buffer;
	};
	let resources = createDocumentResources(base, async () => png(24));
	await Promise.all(
		Array.from({ length: 100 }, () => resources.image("same.png")),
	);
	expect(create).toHaveBeenCalledTimes(1);
	for (let i = 1; i < 64; i++) await resources.image(i + ".png");
	await expect(resources.image("65.png")).rejects.toMatchObject({
		code: "DOCUMENT_RESOURCE_LIMIT",
	});
	resources.dispose();
	resources = createDocumentResources(base, async () =>
		png(10 * 1024 * 1024 + 1),
	);
	await expect(resources.image("big.png")).rejects.toMatchObject({
		code: "DOCUMENT_TOO_LARGE",
	});
	resources.dispose();
	resources = createDocumentResources(base, async () => png(8 * 1024 * 1024));
	await resources.image("a.png");
	await resources.image("b.png");
	await expect(resources.image("c.png")).rejects.toMatchObject({
		code: "DOCUMENT_RESOURCE_LIMIT",
	});
	resources.dispose();
	create.mockRestore();
	revoke.mockRestore();
});

it("transports only verified raster bytes inline for an opaque HTML origin, rejecting supplied data URLs", async () => {
	const png = new Uint8Array(24);
	png.set([137, 80, 78, 71, 13, 10, 26, 10]);
	new DataView(png.buffer).setUint32(16, 1);
	new DataView(png.buffer).setUint32(20, 1);
	const load = vi.fn(async () => png.buffer);
	const resources = createDocumentResources(base, load, "inline-raster");
	expect(await resources.image("pic.png")).toMatch(/^data:image\/png;base64,/);
	expect(
		await resources.image('data:image/svg+xml,<svg onload="x()"/>'),
	).toBeNull();
	expect(load).toHaveBeenCalledTimes(1);
	resources.dispose();
});

it.each([false, true])(
	"stops reading after the total budget is exhausted (parallel=%s)",
	async (parallel) => {
		let bytes = 0;
		const limits: number[] = [];
		const signals: AbortSignal[] = [];
		const load = vi.fn(
			async (
				target: { path: string },
				options: { maxBytes: number; signal: AbortSignal },
			) => {
				limits.push(options?.maxBytes);
				signals.push(options?.signal);
				const size = /[45]\.png$/.test(target.path)
					? 2 * 1024 * 1024
					: 8 * 1024 * 1024;
				if (options && size > options.maxBytes)
					throw Object.assign(new Error("too large"), {
						code: "DOCUMENT_TOO_LARGE",
					});
				bytes += size;
				return new ArrayBuffer(size);
			},
		);
		const resources = createDocumentResources(base, load);
		const read = (i: number) => resources.image(`${i}.png`).catch(() => null);
		if (parallel)
			await Promise.all(Array.from({ length: 8 }, (_, i) => read(i)));
		else for (let i = 0; i < 8; i++) await read(i);
		expect(bytes).toBeLessThanOrEqual(20 * 1024 * 1024);
		expect(load).toHaveBeenCalledTimes(6);
		expect(limits).toEqual([10, 10, 4, 4, 4, 2].map((n) => n * 1024 * 1024));
		expect(bytes).toBe(20 * 1024 * 1024);
		expect(signals.every((signal) => signal?.aborted)).toBe(true);
		expect(resources.limited()).toBe(true);
		resources.dispose();
	},
);

it("cancels the active read and never starts queued reads when disposed", async () => {
	let signal: AbortSignal | undefined;
	const load = vi.fn(
		(_target: unknown, options: { maxBytes: number; signal: AbortSignal }) =>
			new Promise<ArrayBuffer>((_resolve, reject) => {
				signal = options?.signal;
				signal?.addEventListener(
					"abort",
					() => reject(new DOMException("Aborted", "AbortError")),
					{ once: true },
				);
			}),
	);
	const resources = createDocumentResources(base, load);
	const requests = Promise.allSettled([
		resources.image("a.png"),
		resources.image("b.png"),
	]);
	await vi.waitFor(() => expect(load).toHaveBeenCalled());
	resources.dispose();
	expect(signal?.aborted).toBe(true);
	expect((await requests).every((result) => result.status === "rejected")).toBe(
		true,
	);
	expect(load).toHaveBeenCalledTimes(1);
});

it.each([false, true])(
	"keeps loading small images after a single oversized resource (parallel=%s)",
	async (parallel) => {
		const create = vi
			.spyOn(URL, "createObjectURL")
			.mockReturnValue("blob:small");
		const png = new Uint8Array(24);
		png.set([137, 80, 78, 71, 13, 10, 26, 10]);
		new DataView(png.buffer).setUint32(16, 1);
		new DataView(png.buffer).setUint32(20, 1);
		let transferred = 0;
		const load = vi.fn(
			async (
				target: { path: string },
				{ signal }: { maxBytes: number; signal: AbortSignal },
			) => {
				if (target.path.endsWith("large.png"))
					throw Object.assign(new Error("too large"), {
						code: "DOCUMENT_TOO_LARGE",
					});
				expect(signal.aborted).toBe(false);
				transferred += png.byteLength;
				return png.buffer;
			},
		);
		const resources = createDocumentResources(base, load);
		if (parallel) {
			const results = await Promise.allSettled([
				resources.image("large.png"),
				resources.image("small.png"),
			]);
			expect(results[0]).toMatchObject({
				status: "rejected",
				reason: { code: "DOCUMENT_TOO_LARGE" },
			});
			expect(results[1]).toEqual({ status: "fulfilled", value: "blob:small" });
		} else {
			await expect(resources.image("large.png")).rejects.toMatchObject({
				code: "DOCUMENT_TOO_LARGE",
			});
			expect(await resources.image("small.png")).toBe("blob:small");
		}
		expect(transferred).toBe(png.byteLength);
		resources.dispose();
		create.mockRestore();
	},
);
