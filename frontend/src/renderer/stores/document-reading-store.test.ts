import { describe, expect, it } from "vitest";
import { createDocumentReadingStore } from "./document-reading-store";
import type { ResolvedDocument } from "../lib/document-targets";
const document = (name: string, fragment?: string): ResolvedDocument => ({
	target: {
		kind: "shared",
		projectId: "p",
		path: name,
		...(fragment === undefined ? {} : { fragment }),
	},
	canonicalUri: "ao://projects/p/shared/" + name,
	localPath: "/managed/" + name,
	size: 10,
	mediaType: "text/markdown",
	fingerprint: "v1",
});
describe("temporary document reading", () => {
	it("keeps at most20 distinct documents with per-visit positions and Forward branching", () => {
		const store = createDocumentReadingStore();
		for (let n = 0; n < 22; n++)
			store.getState().show("chat", document(n + ".md"));
		expect(store.getState().sessions.chat.history).toHaveLength(20);
		store.getState().savePosition("chat", { scrollTop: 400, zoom: 1.5 });
		store.getState().back("chat");
		expect(store.getState().sessions.chat.index).toBe(18);
		store.getState().forward("chat");
		expect(store.getState().sessions.chat.history[19].position).toEqual({
			scrollTop: 400,
			zoom: 1.5,
		});
		store.getState().show("chat", document("21.md", "heading"));
		expect(store.getState().sessions.chat.history).toHaveLength(20);
		store.getState().back("chat");
		store.getState().show("chat", document("new.md"));
		expect(
			store.getState().sessions.chat.history.at(-1)?.document.target.path,
		).toBe("new.md");
		expect(
			store
				.getState()
				.sessions.chat.history.some((v) => v.document.target.path === "21.md"),
		).toBe(false);
	});
	it("ignores superseded and closed requests and keeps empty history navigation valid", () => {
		const store = createDocumentReadingStore();
		const input = document("a.md").target;
		store.getState().open("chat", input);
		const first = store.getState().sessions.chat.opening!.id;
		store.getState().back("chat");
		expect(store.getState().sessions.chat.index).toBe(-1);
		store.getState().open("chat", input);
		const second = store.getState().sessions.chat.opening!.id;
		store.getState().complete("chat", first, document("old.md"));
		expect(store.getState().sessions.chat.history).toEqual([]);
		store.getState().complete("chat", second, document("a.md"));
		store.getState().open("chat", input);
		const third = store.getState().sessions.chat.opening!.id;
		store.getState().close("chat");
		store.getState().complete("chat", third, document("late.md"));
		expect(
			store.getState().sessions.chat.history.map((v) => v.document.target.path),
		).toEqual(["a.md"]);
	});

	it("keeps histories isolated, closes foreground on departure, and starts empty after restart", () => {
		const store = createDocumentReadingStore();
		store.getState().show("a", document("a.md"));
		store.getState().show("b", document("b.md"));
		store.getState().close("a");
		expect(store.getState().sessions.a.foreground).toBe(false);
		expect(store.getState().sessions.a.history).toHaveLength(1);
		expect(store.getState().sessions.b.foreground).toBe(true);
		expect(createDocumentReadingStore().getState().sessions).toEqual({});
	});
});

it("restores saved positions on Back and Refresh instead of reapplying an old fragment", () => {
	const store = createDocumentReadingStore();
	const chapter = document("chapter.md", "part");
	store.getState().show("chat", chapter);
	store.getState().savePosition("chat", { scrollTop: 500, zoom: 1.5 });
	store.getState().show("chat", document("other.md"));
	store.getState().back("chat");
	let active = store.getState().sessions.chat;
	expect(active.history[active.index].revealFragment).toBe(false);
	store.getState().open("chat", chapter.target, { preservePosition: true });
	let id = store.getState().sessions.chat.opening!.id;
	store.getState().complete("chat", id, { ...chapter, fingerprint: "v2" });
	active = store.getState().sessions.chat;
	expect(active.history[active.index]).toMatchObject({
		position: { scrollTop: 500, zoom: 1.5 },
		revealFragment: false,
	});
	store.getState().open("chat", chapter.target);
	id = store.getState().sessions.chat.opening!.id;
	store.getState().complete("chat", id, chapter);
	active = store.getState().sessions.chat;
	expect(active.history[active.index].revealFragment).toBe(true);
});
