import { describe, expect, it, vi } from "vitest";
import { createDocumentResources, staticDocumentHtml } from "./document-html";
const base = {
	kind: "shared" as const,
	projectId: "p",
	path: "dir/index.html",
};
describe("opaque static documents", () => {
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

it("shares identical image blobs and enforces per-resource, total and source-count budgets",async()=>{
	const create=vi.spyOn(URL,"createObjectURL").mockReturnValue("blob:owned");const revoke=vi.spyOn(URL,"revokeObjectURL").mockImplementation(()=>{});
	const png=(size:number)=>{const b=new Uint8Array(size);b.set([137,80,78,71,13,10,26,10]);new DataView(b.buffer).setUint32(16,1);new DataView(b.buffer).setUint32(20,1);return b.buffer;};
	let resources=createDocumentResources(base,async()=>png(24));
	await Promise.all(Array.from({length:100},()=>resources.image("same.png")));expect(create).toHaveBeenCalledTimes(1);
	for(let i=1;i<64;i++)await resources.image(i+".png");await expect(resources.image("65.png")).rejects.toMatchObject({code:"DOCUMENT_RESOURCE_LIMIT"});resources.dispose();
	resources=createDocumentResources(base,async()=>png(10*1024*1024+1));await expect(resources.image("big.png")).rejects.toMatchObject({code:"DOCUMENT_RESOURCE_LIMIT"});resources.dispose();
	resources=createDocumentResources(base,async()=>png(8*1024*1024));await resources.image("a.png");await resources.image("b.png");await expect(resources.image("c.png")).rejects.toMatchObject({code:"DOCUMENT_RESOURCE_LIMIT"});resources.dispose();
	create.mockRestore();revoke.mockRestore();
});
