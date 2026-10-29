import { describe, expect, it } from "vitest";
import complete from "../../../test/fixtures/app-diagnostics-complete-v1.json";
import { isAppSnapshot } from "./app-diagnostics";

type Path = (string | number)[];
function fields(value: unknown, path: Path = []): Path[] {
	if (Array.isArray(value)) return value.flatMap((item, index) => fields(item, [...path, index]));
	if (typeof value !== "object" || value === null) return [];
	return Object.entries(value).flatMap(([key, item]) => [[...path, key], ...fields(item, [...path, key])]);
}
function mutate(path: Path, mode: "missing" | "null") {
	const snapshot: unknown = structuredClone(complete);
	let parent = snapshot;
	for (const part of path.slice(0, -1)) parent = Reflect.get(Object(parent), part);
	const key = path.at(-1)!;
	if (mode === "missing") Reflect.deleteProperty(Object(parent), key);
	else Reflect.set(Object(parent), key, null);
	return snapshot;
}
describe("live snapshot required fields shared with Go", () => {
	it("accepts the full schema including valid zero/false/empty fields", () => expect(isAppSnapshot(complete)).toBe(true));
	const cases = fields(complete).map((path) => ({ name: path.join("."), path }));
	it.each(cases)("rejects missing $name", ({ path }) => expect(isAppSnapshot(mutate(path, "missing"))).toBe(false));
	it.each(cases)("rejects null $name", ({ path }) => expect(isAppSnapshot(mutate(path, "null"))).toBe(false));
	it("allows style to be absent only for an unavailable node", () => {
		const snapshot = structuredClone(complete);
		snapshot.collector.geometry.nodes[0].availability = "not_mounted";
		Reflect.deleteProperty(snapshot.collector.geometry.nodes[0], "style");
		expect(isAppSnapshot(snapshot)).toBe(true);
	});
});

it("rejects differently cased aliases of optional wire fields", () => {
	const snapshot = structuredClone(complete);
	const node = snapshot.collector.geometry.nodes[0];
	node.availability = "not_mounted";
	Reflect.set(node, "Style", node.style);
	Reflect.deleteProperty(node, "style");
	expect(isAppSnapshot(snapshot)).toBe(false);
});
