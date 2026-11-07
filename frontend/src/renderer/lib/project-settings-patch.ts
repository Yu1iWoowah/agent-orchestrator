type LeafPatch<T> = T extends readonly unknown[]
	? T
	: T extends object
		? SettingsPatch<T>
		: Exclude<T, undefined> | (undefined extends T ? null : never);
type SettingsPatch<T extends object> = { [K in keyof T]?: LeafPatch<T[K]> };

export function settingsObject(value: unknown): value is Record<string, unknown> {
	return typeof value === "object" && value !== null && !Array.isArray(value);
}

export function sameSetting(a: unknown, b: unknown): boolean {
	if (Array.isArray(a) && Array.isArray(b)) return a.length === b.length && a.every((value, i) => sameSetting(value, b[i]));
	if (settingsObject(a) && settingsObject(b)) {
		const keys = Object.keys(a);
		return keys.length === Object.keys(b).length && keys.every((key) => Object.hasOwn(b, key) && sameSetting(a[key], b[key]));
	}
	return a === b;
}

// Objects merge recursively; arrays replace; null removes a key. The callers
// project their typed UI values here, without copying an entire config to PATCH.
export function settingsPatch<T extends object>(before: T, after: T): SettingsPatch<T> {
	let patch: SettingsPatch<T> = {};
	const previousValues = new Map<string, unknown>(Object.entries(before));
	const nextValues = new Map<string, unknown>(Object.entries(after));
	for (const key of new Set([...previousValues.keys(), ...nextValues.keys()])) {
		const previous = previousValues.get(key);
		const next = nextValues.get(key);
		if (sameSetting(previous, next)) continue;
		if (settingsObject(next)) {
			const nested = settingsPatch(settingsObject(previous) ? previous : {}, next);
			if (Object.keys(nested).length) patch = { ...patch, [key]: nested };
		} else patch = { ...patch, [key]: next === undefined ? null : next };
	}
	return patch;
}

// expected comes from the server snapshot, which can differ from displayed
// fallback values (for example legacy model/effort inherited by a worker).
export function settingsExpected(patch: object, snapshot: unknown, atomic: readonly string[] = [], prefix = ""): Record<string, unknown> {
	let expected: Record<string, unknown> = {};
	for (const [key, value] of Object.entries(patch)) {
		const current = settingsObject(snapshot) && Object.hasOwn(snapshot, key) ? snapshot[key] : undefined;
		const path = prefix ? `${prefix}.${key}` : key;
		expected = { ...expected, [key]: settingsObject(value) && !atomic.includes(path) ? settingsExpected(value, current, atomic, path) : (current ?? null) };
	}
	return expected;
}

export function conflictingSettings(
	patch: object,
	expected: unknown,
	current: unknown,
	atomic: readonly string[] = [],
	prefix = "",
): string[] {
	return Object.entries(patch).flatMap(([key, value]) => {
		const path = prefix ? `${prefix}.${key}` : key;
		const before = settingsObject(expected) && Object.hasOwn(expected, key) ? expected[key] : undefined;
		const now = settingsObject(current) && Object.hasOwn(current, key) ? current[key] : undefined;
		return settingsObject(value) && !atomic.includes(path)
			? conflictingSettings(value, before, now, atomic, path)
			: sameSetting(before ?? null, now ?? null)
				? []
				: [path];
	});
}

export class ProjectSettingsConflict extends Error {
	constructor(readonly paths: string[]) {
		super("PROJECT_SETTINGS_CONFLICT");
	}
}

export function settingsSaveError(code: string | undefined, message: string, details: unknown): Error {
	if (code === "PROJECT_SETTINGS_CONFLICT" && settingsObject(details) && Array.isArray(details.conflicts)) {
		const paths = details.conflicts.flatMap((item: unknown) =>
			settingsObject(item) && typeof item.path === "string" ? [item.path] : [],
		);
		if (paths.length) return new ProjectSettingsConflict(paths);
	}
	return new Error(message);
}

// Start from fresh values and carry only the user's non-conflicting edits.
export function rebaseSettings<T extends object>(before: T, draft: T, latest: T, conflicts: readonly string[], prefix = ""): T {
	let result = { ...latest };
	const previous = new Map<string, unknown>(Object.entries(before));
	const edits = new Map<string, unknown>(Object.entries(draft));
	const fresh = new Map<string, unknown>(Object.entries(latest));
	for (const key of new Set([...previous.keys(), ...edits.keys()])) {
		const path = prefix ? `${prefix}.${key}` : key;
		if (
			sameSetting(previous.get(key), edits.get(key)) ||
			conflicts.some((conflict) => path === conflict || path.startsWith(`${conflict}.`))
		)
			continue;
		const value = edits.get(key);
		if (settingsObject(value)) {
			const old = previous.get(key),
				now = fresh.get(key);
			result = { ...result, [key]: rebaseSettings(settingsObject(old) ? old : {}, value, settingsObject(now) ? now : {}, conflicts, path) };
		} else if (value === undefined) Reflect.deleteProperty(result, key);
		else result = { ...result, [key]: value };
	}
	return result;
}

export function projectDraftConflictPaths(paths: readonly string[]): string[] {
	return paths.flatMap((path) => {
		if (path === "displayName" || path === "defaultBranch") return [path];
		const [root, role, member, field] = path.split(".");
		if (root !== "config" || !role) return [];
		if (role === "reviewers")
			return [
				"reviewerHarness",
				"additionalReviewerHarnesses",
				"reviewerModel",
				"reviewerMode",
				"reviewerEffort",
				"reviewerPermissions",
				"accountSettings.reviewers",
			];
		if (role === "agentAccounts") return [`accountSettings.providers${member ? `.${member}` : ""}`];
		if (role === "worker" || role === "orchestrator") {
			if (member === "agentAccountId") return [`accountSettings.${role}`];
			if (member === "agent") return [`${role}Agent`];
			const fields = field ? [field] : ["model", "mode", "effort", "permissions"];
			return fields.map((name) => `${role}${name.charAt(0).toUpperCase()}${name.slice(1)}`);
		}
		if (role === "trackerIntake") return [`intake${member === "enabled" ? "Enabled" : member === "repo" ? "Repo" : "Assignee"}`];
		if (role === "coder") return [`coder.${member === "parameters" ? "parameters" : "workspaceNamePrefix"}`];
		return [role];
	});
}
