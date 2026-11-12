import type { Page } from "@playwright/test";

import type { UpdateSettings, UpdateStatus } from "../../src/main/update-settings";
import type { AoBridge } from "../../src/preload";
import type { DaemonStatus } from "../../src/shared/daemon-status";
import { coerceUiSettings, DEFAULT_UI_SETTINGS } from "../../src/shared/ui-locale";

// The e2e suite runs the renderer under `dev:web` (VITE_NO_ELECTRON=1) with no
// Electron preload, so `window.ao` is undefined and lib/bridge.ts falls back to
// a browser stub that reports the daemon as permanently "stopped" and the app
// version as "0.0.0-preview". The daemon/version smoke cases (DMN-*, INS-004)
// need a deterministic *ready* daemon and a known version string, so we inject
// a complete `window.ao` before any page script runs — the same seam the real
// Electron preload fills. This is a fake *bridge*, not a fake agent: no worker
// process and no GitHub repo are involved, matching the T0 POD constraints.
//
// In a real Linux pod running the packaged Electron build, `window.ao` is the
// live preload; the injected bridge is only the deterministic stand-in for the
// browser harness.
//
// SCOPE / CAVEAT — renderer smoke, not full e2e. Because `window.ao` (its
// local SSE streams included) and the workspace snapshot are faked here, this harness
// CANNOT catch daemon, storage, API, preload, PTY, or filesystem regressions —
// those are the packaged-app pod gate's job (#2697). In particular,
// `useWorkspaceQuery` reads an already-shaped `WorkspaceSummary` straight from
// `window.__aoFakeAgent.snapshot()`, BYPASSING the generated API client + DTO
// mapping; DTO/client coverage comes from the pod gate + unit tests, never from
// these specs. Treat green here as "the renderer renders the injected state,"
// not "the boundary works."

export type FakeBridgeOptions = {
	/** Version string surfaced by app.getVersion() (Settings > Updates). */
	version?: string;
	/** Daemon lifecycle state the renderer should observe. */
	daemonState?: "ready" | "starting" | "stopped" | "error";
	/** REST port advertised when ready (mock data is served regardless). */
	daemonPort?: number;
	/** Desktop updater state surfaced in Settings > Updates. */
	updateStatus?: UpdateStatus;
	/** Persisted automatic-update policy surfaced in Settings > Updates. */
	updateSettings?: UpdateSettings;
};

export async function installFakeBridge(page: Page, opts: FakeBridgeOptions = {}): Promise<void> {
	const version = opts.version ?? "9.9.9-test";
	const daemonState = opts.daemonState ?? "ready";
	const daemonPort = opts.daemonPort ?? 8080;
	const updateStatus = opts.updateStatus ?? ({ state: "idle" } satisfies UpdateStatus);
	const updateSettings =
		opts.updateSettings ??
		({ enabled: false, channel: "latest", nightlyAck: false, feature: null } satisfies UpdateSettings);
	await page.addInitScript(
		({ version, daemonState, daemonPort, updateStatus, updateSettings }) => {
			const unsubscribe = () => () => undefined;
			const updateListeners = new Set<(status: UpdateStatus) => void>();
			let currentUpdateSettings = updateSettings;
			let currentUpdateStatus = updateStatus;
			const emitUpdateStatus = (next: UpdateStatus) => {
				currentUpdateStatus = next;
				for (const listener of updateListeners) listener(next);
			};
			const status: DaemonStatus =
				daemonState === "ready" ? { state: "ready", port: daemonPort } : { state: daemonState };
			const navState = (viewId: string) => ({
				viewId,
				url: "",
				title: "",
				canGoBack: false,
				canGoForward: false,
				isLoading: false,
			});

			// Full AoBridge surface (mirrors src/preload.ts) so any renderer call
			// resolves — an incomplete object would throw the moment the app touched
			// a missing method.
			const ao = {
					appDiagnostics: {
						getIdentity: async () => ({ documentId: "fixture-document", windowId: "fixture-window", generation: 1, enabled: false, expiresAt: 0 }),
						getConsent: async () => null,
						requestEnable: async () => {
							throw new Error("APP_DIAGNOSTICS_UNSUPPORTED");
						},
						revoke: async () => {
							throw new Error("APP_DIAGNOSTICS_UNSUPPORTED");
						},
						audit: async () => {
							throw new Error("APP_DIAGNOSTICS_UNSUPPORTED");
						},
						getStatus: async () => ({ state: "disabled", reason: "consent_required" }),
						register: async () => ({ documentId: "fixture-document", windowId: "fixture-window", generation: 1, enabled: false, expiresAt: 0 }),
						submit: async () => false,
						onRequest: () => () => undefined,
						onConfigure: () => () => undefined,
						onStatus: () => () => undefined,
						onCancel: () => () => undefined,
					},
					app: {
						getVersion: async () => version,
						chooseDirectory: async () => null,
						checkGitRepository: async () => true,
						openExternal: async () => undefined,
						scanImportFolder: async ({ path }: { path: string }) => ({ path, repos: [] }),
						checkAncestorRepo: async () => undefined,
						getRepositoryBranch: async () => undefined,
						getGitHubLogin: async () => "",
						getCachedGitHubOwners: async () => [],
						refreshGitHubOwners: async () => [],
						checkGitHubRepositoryAvailability: async () => ({ available: true }),
						getPathForFile: () => "",
					onOpenFolderPath: () => () => undefined,
					onNewSessionShortcut: unsubscribe,
					onKeyboardShortcutsHelp: unsubscribe,
					onQuitConfirmRequest: unsubscribe,
					resolveQuitConfirm: () => undefined,
					onNewShellTerminalShortcut: unsubscribe,
					onCloseShellTerminalShortcut: unsubscribe,
					setCloseShellTerminalShortcutEnabled: () => undefined,
					setChatDraftRisk: () => undefined,
					onOpenSettingsShortcut: unsubscribe,
					onPreviousSessionShortcut: unsubscribe,
					onNextSessionShortcut: unsubscribe,
					onPreviousTabShortcut: unsubscribe,
					onNextTabShortcut: unsubscribe,
					onDocumentEscape: unsubscribe,
					onShortcut: unsubscribe,
					onScrollPhase: unsubscribe,
				},
				terminal: {
					saveDroppedFile: async () => "",
					setFocused: () => undefined,
					onFontSizeShortcut: () => () => undefined,
				},
				window: {
					getZoomFactor: async () => 1,
					onZoomFactor: () => () => undefined,
					isMaximized: async () => false,
					onMaximized: () => () => undefined,
					isFullScreen: async () => false,
					onFullScreen: () => () => undefined,
				},
				theme: {
					set: async () => undefined,
					persistTerminal: async () => undefined,
					setWindowBackground: async () => undefined,
					setWindowTranslucent: async () => undefined,
				},
				menu: { action: async () => undefined, notifyShellFocus: () => undefined },
				clipboard: {
					writeText: async () => undefined,
					readText: async () => "",
					writeImage: async () => undefined,
				},
				chatImage: {
					fetch: async () => ({ bytes: new Uint8Array(), mimeType: "image/png" }),
					save: async () => true,
				},
				cloudArtifact: {
					fetch: async () => new Uint8Array(),
					onFrameNavigated: () => () => undefined,
				},
				daemon: {
					getStatus: async () => status,
					start: async () => status,
					stop: async () => ({ state: "stopped" }),
					restart: async () => status,
					onStatus: (listener: (s: typeof status) => void) => {
						listener(status);
						return unsubscribe();
					},
				},
				sideChats: { capture: async () => undefined, confirmClose: async () => undefined, getPendingCloses: async () => [], onPendingClosesChanged: () => () => {} },
				editorHandoff: {
					copyWorkspacePath: async () => undefined,
					getState: async () => ({
						targets: [
							{ id: "cursor" as const, name: "Cursor", kind: "editor" as const },
							{ id: "file-manager" as const, name: "File Manager", kind: "file_manager" as const },
							{ id: "terminal" as const, name: "Terminal", kind: "terminal" as const },
						],
						preferredEditorId: "cursor" as const,
						workspaceAvailable: true,
					}),
					getEditors: async () => ({
						editors: [{ id: "cursor" as const, name: "Cursor", kind: "editor" as const }],
						preferredEditorId: "cursor" as const,
					}),
					setPreferredEditor: async (editorId) => editorId,
					open: async () => ({ id: "cursor" as const, name: "Cursor", kind: "editor" as const }),
					openArtifacts: async () => ({ id: "file-manager" as const, name: "File Manager", kind: "file_manager" as const }),
				},
				telemetry: {
					getBootstrap: async () => null,
					getAnalyticsOptOut: async () => false,
					setAnalyticsOptOut: async (optedOut: boolean) => optedOut,
					onAnalyticsOptOut: () => () => undefined,
					setCloudUser: async () => false,
					getGithubLogin: async () => null,
					getPolicy: async () => ({ eventsEnabled: false, consentGeneration: "e2e", updatedAt: new Date(0).toISOString(), acknowledged: false, consentRenewalRequired: false, state: "applied", environmentVeto: true, durabilitySupported: false, reason: "environment_veto" }),
					setEventsEnabled: async () => ({ eventsEnabled: false, consentGeneration: "e2e", updatedAt: new Date(0).toISOString(), acknowledged: false, consentRenewalRequired: false, state: "applied", environmentVeto: true, durabilitySupported: false, reason: "environment_veto" }),
					onPolicy: () => () => false,
					onClearQueues: () => () => false,
					capture: async () => false,
				},
				browser: {
					reconnectRuntime: async () => undefined,
					getRuntimeState: async () => ({ connected: false, connecting: false, rejected: false }),
					nativeCompositionEnabled: true,
					ensure: async (sessionId: string) => navState(`preview:${sessionId}`),
					setBounds: () => undefined,
					onBoundsApplied: () => () => undefined,
					setOverlayOpen: () => undefined,
					navigate: async ({ viewId }: { viewId: string }) => navState(viewId),
					historySuggestions: async () => [],
					historyFavicon: async () => undefined,
					clear: async (viewId: string) => navState(viewId),
					goBack: async (viewId: string) => navState(viewId),
					goForward: async (viewId: string) => navState(viewId),
					reload: async (viewId: string) => navState(viewId),
					stop: async (viewId: string) => navState(viewId),
					getFindState: async (viewId: string) => ({
						viewId,
						tabId: "t1",
						query: "",
						activeMatchOrdinal: 0,
						matches: 0,
						finalUpdate: true,
					}),
					findInPage: async ({ viewId, query }: { viewId: string; query: string }) => ({
						viewId,
						tabId: "t1",
						query,
						activeMatchOrdinal: 0,
						matches: 0,
						finalUpdate: true,
					}),
					stopFindInPage: async ({ viewId }: { viewId: string }) => ({
						viewId,
						tabId: "t1",
						query: "",
						activeMatchOrdinal: 0,
						matches: 0,
						finalUpdate: true,
					}),
					getTabs: async (viewId: string) => ({
						viewId,
						activeTabId: "t1",
						tabs: [{ id: "t1", url: "", title: "", active: true }],
					}),
					selectTab: async ({ viewId, tabId }: { viewId: string; tabId: string }) => ({
						viewId,
						activeTabId: tabId,
						tabs: [{ id: tabId, url: "", title: "", active: true }],
					}),
					closeTab: async ({ viewId }: { viewId: string; tabId: string }) => ({
						viewId,
						activeTabId: "t1",
						tabs: [{ id: "t1", url: "", title: "", active: true }],
					}),
					openTab: async ({ viewId }: { viewId: string; url?: string }) => ({
						viewId,
						activeTabId: "t1",
						tabs: [{ id: "t1", url: "", title: "", active: true }],
					}),
					getProfile: async (viewId: string) => ({ viewId, profileId: null, temporary: true }),
					showProfileMenu: async () => undefined,
					selectProfile: async () => undefined,
					captureScreenshot: async () => undefined,
					downloads: {
						list: async () => ({ downloads: [] }),
						action: async () => ({ downloads: [] }),
						clear: async () => ({ downloads: [] }),
						onChanged: unsubscribe,
					},
					notifyPanelUsed: () => undefined,
					notifyPanelBlur: () => undefined,
					onFocusLocation: unsubscribe,
					onFindOpen: unsubscribe,
					onReopenClosedTab: unsubscribe,
					onClosePanel: unsubscribe,
					devtools: async (input: { viewId: string }) => ({ viewId: input.viewId, open: false, activeTabId: "" }),
					destroy: () => undefined,
					// Annotation contract (mirrors src/preload.ts): useBrowserView subscribes
					// to these whenever SessionView mounts with window.ao.browser present, so
					// an incomplete browser shape would crash the session-detail/preview specs.
					setAnnotationMode: async () => undefined,
					completeAnnotation: async () => undefined,
					discardAnnotations: async () => undefined,
					annotationAction: async () => undefined,
					onAnnotationSubmit: unsubscribe,
					onAnnotationCancel: unsubscribe,
					onAnnotationState: unsubscribe,
					onNavState: unsubscribe,
					onFindState: unsubscribe,
					onTabsState: unsubscribe,
					onAgentActivity: unsubscribe,
					onRuntimeState: unsubscribe,
					onDevToolsState: unsubscribe,
					onProfileState: unsubscribe,
					onProfileManage: unsubscribe,
					onPageFocus: unsubscribe,
				},
				browserProfiles: {
					list: async () => ({ profiles: [], defaultProfileId: null }),
					create: async (name: string) => {
						const now = new Date().toISOString();
						return { id: `fake-${name}`, name, createdAt: now, updatedAt: now };
					},
					rename: async ({ id, name }: { id: string; name: string }) => {
						const now = new Date().toISOString();
						return { id, name, createdAt: now, updatedAt: now };
					},
					clear: async () => undefined,
					usage: async () => ({ sessions: [] }),
					delete: async () => ({}),
					setDefault: async () => undefined,
					discoverImportSources: async () => ({ sources: [] }),
					import: async () => ({ sourceName: "", entries: [] }),
					onImportProgress: () => () => undefined,
				},
				notifications: {
					show: async () => undefined,
					setBadge: async (_count: number) => undefined,
					devBounce: async () => undefined,
					onClick: unsubscribe,
					onPlaySound: unsubscribe,
					reportSoundFailure: () => undefined,
				},
				tray: {
					setAttentionState: () => undefined,
					onOpenSession: unsubscribe,
				},
				appState: {
					getMigration: async () => ({ status: "completed" }),
					setMigration: async () => undefined,
				},
				cliLink: {
					setEnabled: async () => undefined,
					inspect: async () => ({ ok: false as const, code: "unsupported" as const, message: "unavailable" }),
					link: async () => ({ ok: false as const, code: "unsupported" as const, message: "unavailable" }),
					unlink: async () => ({ ok: false as const, code: "unsupported" as const, message: "unavailable" }),
				},
				updateSettings: {
					get: async () => currentUpdateSettings,
					set: async (next: UpdateSettings) => {
						currentUpdateSettings = next;
					},
					setMacDifferentialUpdates: async (enabled: boolean) => {
						currentUpdateSettings = { ...currentUpdateSettings, macDifferentialUpdates: enabled };
					},
				},
				uiSettings: {
					get: async () => ({ ...DEFAULT_UI_SETTINGS }),
					set: async (settings) => coerceUiSettings({ ...DEFAULT_UI_SETTINGS, ...settings }),
				},
				keybindings: {
					get: async () => ({}),
					set: async (overrides) => overrides,
					setRecording: async () => undefined,
				},
				updates: {
					getStatus: async () => currentUpdateStatus,
					check: async () => undefined,
					returnHome: async () => undefined,
					download: async () => undefined,
					install: async () => undefined,
					isPostUpdateRelaunch: async () => false,
					relaunch: async () => undefined,
					onStatus: (listener: (status: UpdateStatus) => void) => {
						updateListeners.add(listener);
						return () => {
							updateListeners.delete(listener);
						};
					},
					onTelemetry: unsubscribe,
				},
				// UpdatesSection calls featureBuilds.getActive() immediately on mount; an
				// omitted namespace would surface as a swallowed React Query error.
				featureBuilds: {
					list: async () => [],
					getActive: async () => null,
				},
				remotes: {
					list: async () => [],
					importAccountHost: async () => undefined,
					pruneAccountHosts: async () => undefined,
					issueAccountToken: async () => "",
					add: async () => "offline" as const,
					update: async () => "offline" as const,
					remove: async () => undefined,
					connect: async (url: string) => {
						throw new Error(`No fake remote for ${url}`);
					},
					disconnect: async () => undefined,
					computerName: async () => "E2E computer",
					previewUrl: async (_hostId: string, _sessionId: string, sourceUrl: string) => sourceUrl,
					resolvePreviewUrl: async (_hostId: string, _sessionId: string, viewedUrl: string) => viewedUrl,
					agentBrowserUrl: async () => "",
					externalUrl: async (_hostId: string, url: string) => url,
				},
				cloud: {
					getSession: async () => null,
					signIn: async () => undefined,
					signOut: async () => undefined,
					cancelProviderAuth: async () => undefined,
					connectProviderAuth: async () => undefined,
					localAuthAvailable: async () => false,
					onOpenShareLink: () => () => undefined,
					takePendingShareLink: async () => null,
					localRegister: async () => {
						throw new Error("local auth is unavailable in e2e");
					},
					localLogin: async () => {
						throw new Error("local auth is unavailable in e2e");
					},
					onSessionChanged: unsubscribe,
				},
				cloudCp: {
					request: async () => ({ status: 401, headers: {}, body: "" }),
					openStream: async () => ({ streamId: "stream_test" }),
					closeStream: () => undefined,
					onStreamEvent: unsubscribe,
					agentBrowserUrl: async () => "",
				},
				// Local streams open and stay quiet.
				daemonStream: {
					openStream: async () => ({ streamId: "daemon_stream_test" }),
					closeStream: () => undefined,
					onStreamEvent: unsubscribe,
				},
			} satisfies AoBridge;
			(window as unknown as { __aoFakeUpdates: { setStatus: (status: UpdateStatus) => void } }).__aoFakeUpdates = {
				setStatus: emitUpdateStatus,
			};
			(window as unknown as { ao: unknown }).ao = ao;
		},
		{ version, daemonState, daemonPort, updateStatus, updateSettings },
	);
}

// ── Fake-agent timeline harness (FAKE tier) ─────────────────────────────────
//
// The FAKE cases in #2483 need an agent to *do* something over time (spawn a
// session, go active, hit waiting_input, move board columns, stream a terminal,
// raise a needs-input notification). Under the browser harness there is no Go
// daemon and no agent, so we simulate that timeline at the bridge:
//
//   1. A `window.ao` whose daemon is ready on a port — so the renderer sets its
//      REST base URL and opens its SSE streams (the same seam the packaged app
//      fills). The `browser.*` IPC is driven off a shared in-page state so the
//      preview surface is controllable.
//   2. A fake `window.ao.daemonStream` — the daemon's CDC (`/api/v1/events`) and
//      notification (`/api/v1/notifications/stream`) SSE streams, which main
//      relays in the desktop app. The controller pushes `session_updated` /
//      `notification_created` frames into them, which is exactly what drives the
//      renderer's cache-invalidation → refetch path (no manual refresh),
//      matching the real daemon's behaviour.
//   3. A mutable workspace snapshot read by `useWorkspaceQuery` via
//      `window.__aoFakeAgent.snapshot()` (dev:web seam). Controller mutations +
//      an SSE push = the card the renderer repaints.
//
// The real Go fake-agent plugin drives the same states in the later real-daemon
// pod run; here the same specs run against this bridge-level simulation.

export type FakeWorker = {
	id: string;
	title: string;
	createdAt?: string;
	mode?: "chat" | "tui";
	provider?: string;
	branch?: string;
	status?: string;
	activity?: "active" | "idle" | "waiting_input" | "exited";
	previewUrl?: string;
	previewRevision?: number;
};

export type FakeAgentOptions = {
	version?: string;
	daemonPort?: number;
	projectId?: string;
	projectName?: string;
	/** Override navigator.platform (e.g. "Linux …") so platform-gated UI mounts. */
	platform?: string;
	/** Worker sessions present at first paint. */
	workers?: FakeWorker[];
	/** Persist the fixture project's disclosure as expanded before the app mounts. */
	expandProject?: boolean;
	/** Relay local streams from a real HTTP SSE listener on `daemonPort`. */
	nativeEvents?: boolean;
};

/**
 * The in-page fake-agent controller, callable from specs via `page.evaluate`.
 * Normal mutations change the snapshot and push the matching SSE frame, so the
 * renderer repaints through its real invalidation path. Diagnostic controls
 * can deliberately defer or omit a frame to inspect the cache boundary.
 */
export type FakeAgentController = {
	snapshot: () => unknown[];
	createWorker: (worker: FakeWorker) => void;
	createWorkerWithoutEvent: (worker: FakeWorker) => void;
	createWorkerDuringNextSnapshot: (worker: FakeWorker) => void;
	removeWorker: (id: string) => void;
	setStatus: (id: string, status: string, activity?: string) => void;
	conversationChanged: (sessionId: string, conversationId: string) => void;
	metadataChanged: (sessionId: string) => void;
	setTerminalHandle: (id: string, handleId: string) => void;
	/** Moves a session to another agent, as a provider switch does. */
	setProvider: (id: string, provider: string) => void;
	setPreview: (id: string, previewUrl: string, previewRevision?: number) => void;
	setBrowserError: (message: string | null) => void;
	reconnectEventsWithReplayBacklog: (count: number) => void;
	replayProgress: () => FakeReplayProgress;
	/** Drops the CDC stream and refuses its reconnect; resolves once it is CLOSED. */
	closeEventsStream: () => Promise<void>;
	/** `openedAt` is the `Date.now()` at which the stream last fired `open`. */
	eventStreams: () => Array<{ url: string; readyState: number; openedAt: number | null }>;
	startContinuousWorkspaceEvents: (intervalMs: number, eventsPerTick?: number) => void;
	stopContinuousWorkspaceEvents: () => void;
	continuousWorkspaceEvents: () => FakePressureProgress;
	setSnapshotFailures: (count: number) => void;
	snapshotCallCount: () => number;
	notify: (n: { id: string; type: string; title: string; body?: string; sessionId?: string }) => void;
};

/** The current (or last) CDC gap replay: `remaining` frames are still queued,
 *  `abandoned` frames (deferred events included) were dropped when the app
 *  replaced the stream. The counters reset when a new replay starts. */
export type FakeReplayProgress = {
	phase: "idle" | "connecting" | "replaying";
	remaining: number;
	delivered: number;
	abandoned: number;
};

/** Continuous SSE pressure: frames the daemon has `sent` by elapsed time, and
 *  how many of them the renderer has been handed so far. */
export type FakePressureProgress = {
	sent: number;
	delivered: number;
};

export type FakeUpdateController = {
	setStatus: (status: UpdateStatus) => void;
};

declare global {
	interface Window {
		__aoFakeAgent?: FakeAgentController;
		__aoFakeUpdates?: FakeUpdateController;
	}
}

/**
 * Install the fake-agent bridge + SSE + snapshot seam before any page script.
 * Drive the timeline from specs with
 * `page.evaluate(() => window.__aoFakeAgent!.setStatus(...))`.
 */
export async function installFakeAgent(page: Page, opts: FakeAgentOptions = {}): Promise<void> {
	const version = opts.version ?? "9.9.9-test";
	const daemonPort = opts.daemonPort ?? 8080;
	const projectId = opts.projectId ?? "fake-proj";
	const projectName = opts.projectName ?? "fake-proj";
	const platform = opts.platform ?? null;
	const workers = opts.workers ?? [];
	const expandProject = opts.expandProject ?? false;
	const nativeEvents = opts.nativeEvents ?? false;
	// Renderer reload does not create a new daemon session incarnation.
	const nowIso = new Date().toISOString();

	await page.addInitScript(
		({ version, daemonPort, projectId, projectName, platform, workers, nowIso, expandProject, nativeEvents }) => {
			try {
				if (expandProject) {
					const key = "ao.sidebar.expanded-projects";
					const expanded = JSON.parse(localStorage.getItem(key) ?? "[]");
					if (Array.isArray(expanded) && !expanded.includes(projectId)) {
						localStorage.setItem(key, JSON.stringify([...expanded, projectId]));
					}
				}
			} catch {
				/* storage can be unavailable in restricted browser contexts */
			}
			if (platform) {
				try {
					Object.defineProperty(navigator, "platform", { get: () => platform, configurable: true });
				} catch {
					/* ignore */
				}
			}

			type Session = Record<string, unknown>;
			// The daemon derives the board lane; the fake stands in for it so
			// driving a spec's status through setStatus still moves the card.
			const kanbanColumnFor = (status: string): string => {
				switch (status) {
					case "merged":
					case "approved":
					case "mergeable":
						return "ready";
					case "terminated":
						return "archive";
					case "review_pending":
					case "pr_open":
					case "draft":
						return "validating";
					case "needs_input":
					case "exited":
					case "no_signal":
					case "ci_failed":
					case "changes_requested":
						return "needs_review";
					default:
						return "building";
				}
			};
			const makeWorker = (w: (typeof workers)[number]): Session => ({
				id: w.id,
				terminalHandleId: (w.mode ?? "tui") === "tui" ? `${w.id}/terminal_0` : undefined,
				workspaceId: projectId,
				workspaceName: projectName,
				title: w.title,
				provider: w.provider ?? "codex",
				kind: "worker",
				mode: w.mode ?? "tui",
				branch: w.branch ?? `session/${w.id}`,
				status: w.status ?? "working",
				kanbanColumn: kanbanColumnFor(w.status ?? "working"),
				createdAt: w.createdAt ?? nowIso,
				updatedAt: new Date().toISOString(),
				activity: { state: w.activity ?? "active", lastActivityAt: new Date().toISOString() },
				previewUrl: w.previewUrl,
				previewRevision: w.previewRevision,
				prs: [],
			});

			const project: Record<string, unknown> = {
				id: projectId,
				name: projectName,
				kind: "single_repo",
				path: `/repos/${projectName}`,
				type: "main",
				orchestratorAgent: "codex",
				sessions: [
					{
						id: `${projectId}-orchestrator`,
						terminalHandleId: `${projectId}-orchestrator/terminal_0`,
						workspaceId: projectId,
						workspaceName: projectName,
						title: "Project orchestrator",
						provider: "codex",
						kind: "orchestrator",
						branch: "main",
						status: "working",
						kanbanColumn: "building",
						createdAt: nowIso,
						updatedAt: nowIso,
						activity: { state: "active", lastActivityAt: nowIso },
						prs: [],
					},
					...workers.map(makeWorker),
				],
			};

			type FakeStreamEvent = { type: "chunk"; data: string } | { type: "end" } | { type: "error"; message: string };
			// A local stream the app opened through the preload `daemonStream`
			// bridge, as main would hold it. `readyState` is 1 while the daemon side
			// is open and 2 once it is closed by either end.
			interface FakeStream {
				id: string;
				url: string;
				readyState: number;
				openedAt: number | null;
				listener?: (event: FakeStreamEvent) => void;
				sendCursor: boolean;
				abort?: () => void;
			}

			const state = {
				browserError: null as string | null,
				eventSources: [] as FakeStream[],
				streamSeq: 0,
				// Refuses the next CDC open: closeEventsStream's failed reconnect.
				refuseNextCdcOpen: undefined as undefined | (() => void),
				workspaces: [project],
				replay: {
					phase: "idle" as "idle" | "connecting" | "replaying",
					source: undefined as FakeStream | undefined,
					remaining: 0,
					delivered: 0,
					abandoned: 0,
					deferred: [] as Array<{ type: string; data?: string }>,
				},
				raceWorker: null as (typeof workers)[number] | null,
				cdcSeq: 0,
				pressure: {
					timer: undefined as number | undefined,
					startedAt: 0,
					stoppedAt: undefined as number | undefined,
					periodMs: 1,
					perTick: 1,
					delivered: 0,
				},
				snapshotCalls: 0,
				snapshotFailures: 0,
			};

			const findSession = (id: string) => (project.sessions as Session[]).find((s) => s.id === id);
			const addWorker = (worker: (typeof workers)[number]) => {
				if (!findSession(worker.id)) (project.sessions as Session[]).push(makeWorker(worker));
			};
			const touch = (s: Session) => {
				s.updatedAt = new Date().toISOString();
			};

			const isCdc = (path: string) => path.startsWith("/api/v1/events");
			// The daemon's SSE frame. CDC frames carry the sequence as their id, so
			// the app's stream resends it as Last-Event-ID when it reconnects.
			const send = (stream: FakeStream, type: string, data = "") => {
				const id = isCdc(stream.url) ? `id: ${state.cdcSeq}\n` : "";
				stream.listener?.({ type: "chunk", data: `${id}event: ${type}\ndata: ${data}\n\n` });
			};
			// The daemon drops the connection; the app's stream reconnects on its own.
			const dropStream = (stream: FakeStream) => {
				stream.readyState = 2;
				stream.listener?.({ type: "error", message: "The daemon closed the stream." });
			};
			const findStream = (streamId: string) => state.eventSources.find((stream) => stream.id === streamId);

			// With nativeEvents the stream comes from a real HTTP SSE listener on
			// the daemon port, read with fetch the way main reads it.
			const openRealStream = async (init: { path: string; lastEventId?: string }) => {
				const headers: Record<string, string> = { Accept: "text/event-stream" };
				if (init.lastEventId !== undefined) headers["Last-Event-ID"] = init.lastEventId;
				const controller = new AbortController();
				const response = await fetch(`http://127.0.0.1:${daemonPort}${init.path}`, { headers, signal: controller.signal });
				if (!response.ok || response.body === null) throw new Error(`DAEMON_STREAM_ERROR ${response.status} The stream was refused.`);
				const stream: FakeStream = {
					id: `fake-stream-${(state.streamSeq += 1)}`,
					url: init.path,
					readyState: 1,
					openedAt: Date.now(),
					sendCursor: false,
					abort: () => controller.abort(),
				};
				state.eventSources.push(stream);
				const reader = response.body.getReader();
				const decoder = new TextDecoder();
				setTimeout(async () => {
					try {
						for (;;) {
							const { done, value } = await reader.read();
							if (done) break;
							stream.listener?.({ type: "chunk", data: decoder.decode(value, { stream: true }) });
						}
						stream.listener?.({ type: "end" });
					} catch (error) {
						if (stream.readyState === 1) stream.listener?.({ type: "error", message: String(error) });
					}
					stream.readyState = 2;
				}, 0);
				return { streamId: stream.id };
			};

			const daemonStream = {
				openStream: async (init: { path: string; lastEventId?: string }) => {
					if (isCdc(init.path) && state.refuseNextCdcOpen) {
						const refused = state.refuseNextCdcOpen;
						state.refuseNextCdcOpen = undefined;
						setTimeout(refused, 0);
						throw new Error("DAEMON_STREAM_ERROR 503 The daemon refused the stream.");
					}
					if (nativeEvents) return openRealStream(init);
					const stream: FakeStream = {
						id: `fake-stream-${(state.streamSeq += 1)}`,
						url: init.path,
						readyState: 1,
						openedAt: Date.now(),
						// `?after=latest` starts with a cursor frame, as the daemon sends.
						sendCursor: isCdc(init.path) && init.lastEventId === undefined,
					};
					state.eventSources.push(stream);
					if (isCdc(init.path) && state.replay.phase === "connecting") {
						// Only the stream's own reconnect resends Last-Event-ID and gets the
						// gap; a stream the app constructs starts at head.
						if (init.lastEventId !== undefined) startReplay(stream);
						else abandonReplay();
					}
					return { streamId: stream.id };
				},
				closeStream: (streamId: string) => {
					const stream = findStream(streamId);
					if (!stream) return;
					stream.readyState = 2;
					stream.abort?.();
				},
				onStreamEvent: (streamId: string, listener: (event: FakeStreamEvent) => void) => {
					const stream = findStream(streamId);
					if (!stream) return () => undefined;
					stream.listener = listener;
					if (stream.sendCursor) {
						stream.sendCursor = false;
						setTimeout(() => {
							if (stream.readyState === 1) send(stream, "cursor", "{}");
						}, 0);
					}
					return () => {
						if (stream.listener === listener) stream.listener = undefined;
					};
				},
			};

			const emit = (match: string, type: string, data?: string) => {
				// Events committed while the CDC stream reconnects reach it through the
				// Last-Event-ID replay, after the gap it is still catching up on.
				if (match === "/api/v1/events" && state.replay.phase !== "idle") {
					state.replay.deferred.push({ type, data });
					return;
				}
				for (const stream of state.eventSources) {
					if (!stream.url.includes(match) || stream.readyState !== 1) continue;
					send(stream, type, data);
				}
			};
			const pushWorkspaces = (type = "session_updated") => emit("/api/v1/events", type);

			// The app opens the CDC stream as `/api/v1/events?after=latest`.
			const liveCdcStream = () =>
				[...state.eventSources]
					.reverse()
					.find((source) => source.url.includes("/api/v1/events") && source.readyState !== 2);
			// The daemon's SSE `data`: the full CDC envelope (backend/internal/cdc/event.go).
			const cdcFrame = (type: string, session: Session) =>
				JSON.stringify({
					seq: (state.cdcSeq += 1),
					projectId,
					sessionId: session.id,
					type,
					payload: { id: session.id, activity: "active", isTerminated: false },
					createdAt: nowIso,
				});
			// The daemon replays and flushes in 512-event pages (httpd/events.go).
			const REPLAY_PAGE = 512;
			// Session events come in runs from the few workers active at a time, not
			// round-robin over every session: about 10 sessions per daemon page.
			const ACTIVE_RUN = 50;
			const activeSession = (sessions: Session[], n: number) => sessions[Math.floor(n / ACTIVE_RUN) % sessions.length];
			const pressureSent = () => {
				const { startedAt, stoppedAt, periodMs, perTick } = state.pressure;
				if (!startedAt) return 0;
				return Math.floor(((stoppedAt ?? performance.now()) - startedAt) / periodMs) * perTick;
			};
			const stopPressure = () => {
				const pressure = state.pressure;
				if (pressure.timer === undefined) return;
				window.clearInterval(pressure.timer);
				pressure.timer = undefined;
				pressure.stoppedAt = performance.now();
			};

			// Only the stream's own reconnect replays a gap: it resends Last-Event-ID,
			// which overrides `?after=latest`. A stream the app constructs (its retry
			// after CLOSED, or a daemon/base-URL change) starts at head, so when the
			// app replaces this stream the rest of the gap is never delivered.
			const abandonReplay = () => {
				const replay = state.replay;
				replay.abandoned += replay.remaining + replay.deferred.length;
				replay.remaining = 0;
				replay.deferred = [];
				replay.phase = "idle";
			};
			// The gap holds history from before it began, not sessions created since.
			let gapSessions: Session[] = [];
			const startReplay = (stream: FakeStream) => {
				const replay = state.replay;
				replay.source = stream;
				replay.phase = "replaying";
				const drainPage = () => {
					if (stream.readyState !== 1) return abandonReplay();
					const frames = Math.min(REPLAY_PAGE, replay.remaining);
					for (let i = 0; i < frames; i += 1) {
						send(stream, "session_updated", cdcFrame("session_updated", activeSession(gapSessions, replay.delivered + i)));
					}
					replay.remaining -= frames;
					replay.delivered += frames;
					if (replay.remaining > 0) {
						setTimeout(drainPage, 0);
						return;
					}
					// Events committed after the gap began follow it in sequence order.
					for (const frame of replay.deferred) send(stream, frame.type, frame.data);
					replay.deferred = [];
					replay.phase = "idle";
				};
				// The app subscribes as soon as its open resolves, and fires `open`
				// before the replayed frames arrive.
				setTimeout(drainPage, 0);
			};
			const reconnectWithReplay = (source: FakeStream, count: number) => {
				const replay = state.replay;
				if (replay.phase !== "idle") throw new Error("a CDC replay is already in progress");
				gapSessions = [...(project.sessions as Session[])];
				replay.phase = "connecting";
				replay.source = source;
				replay.remaining = count;
				replay.delivered = 0;
				replay.abandoned = 0;
				dropStream(source);
			};

			const createWorker = (worker: (typeof workers)[number]) => {
				addWorker(worker);
				emit("/api/v1/events", "session_created", cdcFrame("session_created", findSession(worker.id)!));
			};

			const controller: FakeAgentController = {
				snapshot: () => {
					state.snapshotCalls += 1;
					if (state.snapshotFailures > 0) {
						state.snapshotFailures -= 1;
						throw new Error("simulated workspace sessions response failure");
					}
					const snapshot = JSON.parse(JSON.stringify(state.workspaces));
					const raceWorker = state.raceWorker;
					if (raceWorker) {
						state.raceWorker = null;
						createWorker(raceWorker);
					}
					return snapshot;
				},
				createWorker,
				createWorkerWithoutEvent: addWorker,
				createWorkerDuringNextSnapshot: (w) => { state.raceWorker = w; },
				removeWorker: (id) => {
					const sessions = project.sessions as Session[];
					const index = sessions.findIndex((session) => session.id === id);
					if (index >= 0) sessions.splice(index, 1);
					pushWorkspaces();
				},
				setStatus: (id, status, activity) => {
					const s = findSession(id);
					if (!s) return;
					s.status = status;
					s.kanbanColumn = kanbanColumnFor(status);
					s.displayStatus = undefined;
					if (activity) s.activity = { state: activity, lastActivityAt: new Date().toISOString() };
					touch(s);
					pushWorkspaces();
				},
				conversationChanged: (sessionId, conversationId) => {
					emit("/api/v1/events", "session_updated", JSON.stringify({ type: "session_updated", sessionId, payload: { conversationId } }));
				},
				metadataChanged: (sessionId) => {
					emit("/api/v1/events", "session_updated", JSON.stringify({ type: "session_updated", sessionId, payload: { id: sessionId } }));
				},
				setTerminalHandle: (id, handleId) => {
					const s = findSession(id);
					if (!s) return;
					s.terminalHandleId = handleId;
					touch(s);
					pushWorkspaces();
				},
				setProvider: (id, provider) => {
					const s = findSession(id);
					if (!s) return;
					s.provider = provider;
					touch(s);
					pushWorkspaces();
				},
				setPreview: (id, previewUrl, previewRevision) => {
					const s = findSession(id);
					if (!s) return;
					s.previewUrl = previewUrl;
					s.previewRevision = previewRevision ?? (typeof s.previewRevision === "number" ? s.previewRevision : 0) + 1;
					touch(s);
					pushWorkspaces();
				},
				setBrowserError: (message) => {
					state.browserError = message;
				},
				reconnectEventsWithReplayBacklog: (count) => {
					const source = liveCdcStream();
					if (!source) throw new Error("no open CDC stream to reconnect");
					reconnectWithReplay(source, Math.max(0, Math.floor(count)));
				},
				replayProgress: () => {
					const { phase, remaining, delivered, abandoned } = state.replay;
					return { phase, remaining, delivered, abandoned };
				},
				// The daemon drops the stream and refuses its reconnect, so it ends
				// CLOSED and event-transport schedules its own retry with a fresh
				// stream. Resolves once the reconnect has been refused.
				closeEventsStream: () => {
					const source = liveCdcStream();
					if (!source) throw new Error("no open CDC stream to close");
					return new Promise<void>((resolve) => {
						state.refuseNextCdcOpen = resolve;
						dropStream(source);
					});
				},
				eventStreams: () =>
					state.eventSources
						.filter((source) => source.url.includes("/api/v1/events"))
						.map(({ url, readyState, openedAt }) => ({ url, readyState, openedAt })),
				startContinuousWorkspaceEvents: (intervalMs, eventsPerTick = 1) => {
					stopPressure();
					const pressure = state.pressure;
					pressure.startedAt = performance.now();
					pressure.stoppedAt = undefined;
					pressure.periodMs = Math.max(1, Math.floor(intervalMs));
					pressure.perTick = Math.max(1, Math.floor(eventsPerTick));
					pressure.delivered = 0;
					const sessions = [...(project.sessions as Session[])];
					let draining = false;
					// The daemon keeps sending while rendering blocks the main thread, so
					// frames come due by elapsed time, not by how often this timer runs.
					// The renderer handles each delivery as its own task: hand over the
					// backlog one tick's worth at a time and yield between chunks, so fetch
					// resolution and rendering can run in between, as in production.
					const drain = () => {
						if (pressure.timer !== timer) {
							draining = false;
							return;
						}
						const sent = pressureSent();
						const end = Math.min(sent, pressure.delivered + pressure.perTick);
						for (; pressure.delivered < end; pressure.delivered += 1) {
							emit("/api/v1/events", "session_updated", cdcFrame("session_updated", activeSession(sessions, pressure.delivered)));
						}
						if (pressure.delivered < sent) setTimeout(drain, 0);
						else draining = false;
					};
					const timer = window.setInterval(() => {
						if (draining) return;
						draining = true;
						drain();
					}, pressure.periodMs);
					pressure.timer = timer;
				},
				stopContinuousWorkspaceEvents: stopPressure,
				continuousWorkspaceEvents: () => ({ sent: pressureSent(), delivered: state.pressure.delivered }),
				setSnapshotFailures: (count) => { state.snapshotFailures = Math.max(0, Math.floor(count)); },
				snapshotCallCount: () => state.snapshotCalls,
				notify: (n) => {
					const payload = JSON.stringify({
						id: n.id,
						type: n.type,
						title: n.title,
						body: n.body ?? "",
						sessionId: n.sessionId ?? "",
						projectId,
						target: { kind: "session", sessionId: n.sessionId ?? "" },
						status: "unread",
						createdAt: new Date().toISOString(),
					});
					emit("/api/v1/notifications/stream", "notification_created", payload);
				},
			};
			(window as unknown as { __aoFakeAgent: unknown }).__aoFakeAgent = controller;

			const unsubscribe = () => () => undefined;
			const status: DaemonStatus = { state: "ready", port: daemonPort };
			const navState = (viewId: string, url = "", error?: string) => ({
				viewId,
				url,
				title: url ? "AO preview" : "",
				canGoBack: false,
				canGoForward: false,
				isLoading: false,
				...(error ? { error } : {}),
			});
			const ao = {
					appDiagnostics: {
						getIdentity: async () => ({ documentId: "fixture-document", windowId: "fixture-window", generation: 1, enabled: false, expiresAt: 0 }),
						getConsent: async () => null,
						requestEnable: async () => {
							throw new Error("APP_DIAGNOSTICS_UNSUPPORTED");
						},
						revoke: async () => {
							throw new Error("APP_DIAGNOSTICS_UNSUPPORTED");
						},
						audit: async () => {
							throw new Error("APP_DIAGNOSTICS_UNSUPPORTED");
						},
						getStatus: async () => ({ state: "disabled", reason: "consent_required" }),
						register: async () => ({ documentId: "fixture-document", windowId: "fixture-window", generation: 1, enabled: false, expiresAt: 0 }),
						submit: async () => false,
						onRequest: () => () => undefined,
						onConfigure: () => () => undefined,
						onStatus: () => () => undefined,
						onCancel: () => () => undefined,
					},
					app: {
						getVersion: async () => version,
						chooseDirectory: async () => null,
						checkGitRepository: async () => true,
						openExternal: async () => undefined,
						scanImportFolder: async ({ path }: { path: string }) => ({ path, repos: [] }),
						checkAncestorRepo: async () => undefined,
						getRepositoryBranch: async () => undefined,
						getGitHubLogin: async () => "",
						getCachedGitHubOwners: async () => [],
						refreshGitHubOwners: async () => [],
						checkGitHubRepositoryAvailability: async () => ({ available: true }),
						getPathForFile: () => "",
					onOpenFolderPath: () => () => undefined,
					onNewSessionShortcut: unsubscribe,
					onKeyboardShortcutsHelp: unsubscribe,
					onQuitConfirmRequest: unsubscribe,
					resolveQuitConfirm: () => undefined,
					onNewShellTerminalShortcut: unsubscribe,
					onCloseShellTerminalShortcut: unsubscribe,
					setCloseShellTerminalShortcutEnabled: () => undefined,
					setChatDraftRisk: () => undefined,
					onOpenSettingsShortcut: unsubscribe,
					onPreviousSessionShortcut: unsubscribe,
					onNextSessionShortcut: unsubscribe,
					onPreviousTabShortcut: unsubscribe,
					onNextTabShortcut: unsubscribe,
					onDocumentEscape: unsubscribe,
					onShortcut: unsubscribe,
					onScrollPhase: unsubscribe,
				},
				terminal: {
					saveDroppedFile: async () => "",
					setFocused: () => undefined,
					onFontSizeShortcut: () => () => undefined,
				},
				window: {
					getZoomFactor: async () => 1,
					onZoomFactor: () => () => undefined,
					isMaximized: async () => false,
					onMaximized: () => () => undefined,
					isFullScreen: async () => false,
					onFullScreen: () => () => undefined,
				},
				theme: {
					set: async () => undefined,
					persistTerminal: async () => undefined,
					setWindowBackground: async () => undefined,
					setWindowTranslucent: async () => undefined,
				},
				menu: { action: async () => undefined, notifyShellFocus: () => undefined },
				clipboard: { writeText: async () => undefined, readText: async () => "", writeImage: async () => undefined },
				chatImage: {
					fetch: async () => ({ bytes: new Uint8Array(), mimeType: "image/png" }),
					save: async () => true,
				},
				cloudArtifact: {
					fetch: async () => new Uint8Array(),
					onFrameNavigated: () => () => undefined,
				},
				daemon: {
					getStatus: async () => status,
					start: async () => status,
					stop: async () => ({ state: "stopped" }),
					restart: async () => status,
					onStatus: (listener: (s: typeof status) => void) => {
						listener(status);
						return unsubscribe();
					},
				},
				sideChats: { capture: async () => undefined, confirmClose: async () => undefined, getPendingCloses: async () => [], onPendingClosesChanged: () => () => {} },
				editorHandoff: {
					copyWorkspacePath: async () => undefined,
					getState: async () => ({
						targets: [
							{ id: "cursor" as const, name: "Cursor", kind: "editor" as const },
							{ id: "file-manager" as const, name: "File Manager", kind: "file_manager" as const },
							{ id: "terminal" as const, name: "Terminal", kind: "terminal" as const },
						],
						preferredEditorId: "cursor" as const,
						workspaceAvailable: true,
					}),
					getEditors: async () => ({
						editors: [{ id: "cursor" as const, name: "Cursor", kind: "editor" as const }],
						preferredEditorId: "cursor" as const,
					}),
					setPreferredEditor: async (editorId) => editorId,
					open: async () => ({ id: "cursor" as const, name: "Cursor", kind: "editor" as const }),
					openArtifacts: async () => ({ id: "file-manager" as const, name: "File Manager", kind: "file_manager" as const }),
				},
				telemetry: {
					getBootstrap: async () => null,
					getAnalyticsOptOut: async () => false,
					setAnalyticsOptOut: async (optedOut: boolean) => optedOut,
					onAnalyticsOptOut: () => () => undefined,
					setCloudUser: async () => false,
					getGithubLogin: async () => null,
					getPolicy: async () => ({ eventsEnabled: false, consentGeneration: "e2e", updatedAt: new Date(0).toISOString(), acknowledged: false, consentRenewalRequired: false, state: "applied", environmentVeto: true, durabilitySupported: false, reason: "environment_veto" }),
					setEventsEnabled: async () => ({ eventsEnabled: false, consentGeneration: "e2e", updatedAt: new Date(0).toISOString(), acknowledged: false, consentRenewalRequired: false, state: "applied", environmentVeto: true, durabilitySupported: false, reason: "environment_veto" }),
					onPolicy: () => () => false,
					onClearQueues: () => () => false,
					capture: async () => false,
				},
				browser: {
					reconnectRuntime: async () => undefined,
					getRuntimeState: async () => ({ connected: false, connecting: false, rejected: false }),
					nativeCompositionEnabled: true,
					ensure: async (sessionId: string) => navState(`preview:${sessionId}`),
					setBounds: () => undefined,
					onBoundsApplied: () => () => undefined,
					setOverlayOpen: () => undefined,
					navigate: async ({ viewId, url }: { viewId: string; url: string }) =>
						state.browserError ? navState(viewId, "", state.browserError) : navState(viewId, url),
					historySuggestions: async () => [],
					historyFavicon: async () => undefined,
					clear: async (viewId: string) => navState(viewId),
					goBack: async (viewId: string) => navState(viewId),
					goForward: async (viewId: string) => navState(viewId),
					reload: async (viewId: string) => navState(viewId),
					stop: async (viewId: string) => navState(viewId),
					getFindState: async (viewId: string) => ({
						viewId,
						tabId: "t1",
						query: "",
						activeMatchOrdinal: 0,
						matches: 0,
						finalUpdate: true,
					}),
					findInPage: async ({ viewId, query }: { viewId: string; query: string }) => ({
						viewId,
						tabId: "t1",
						query,
						activeMatchOrdinal: 0,
						matches: 0,
						finalUpdate: true,
					}),
					stopFindInPage: async ({ viewId }: { viewId: string }) => ({
						viewId,
						tabId: "t1",
						query: "",
						activeMatchOrdinal: 0,
						matches: 0,
						finalUpdate: true,
					}),
					getTabs: async (viewId: string) => ({
						viewId,
						activeTabId: "t1",
						tabs: [{ id: "t1", url: "", title: "", active: true }],
					}),
					selectTab: async ({ viewId, tabId }: { viewId: string; tabId: string }) => ({
						viewId,
						activeTabId: tabId,
						tabs: [{ id: tabId, url: "", title: "", active: true }],
					}),
					closeTab: async ({ viewId }: { viewId: string; tabId: string }) => ({
						viewId,
						activeTabId: "t1",
						tabs: [{ id: "t1", url: "", title: "", active: true }],
					}),
					openTab: async ({ viewId }: { viewId: string; url?: string }) => ({
						viewId,
						activeTabId: "t1",
						tabs: [{ id: "t1", url: "", title: "", active: true }],
					}),
					getProfile: async (viewId: string) => ({ viewId, profileId: null, temporary: true }),
					showProfileMenu: async () => undefined,
					selectProfile: async () => undefined,
					captureScreenshot: async () => undefined,
					downloads: {
						list: async () => ({ downloads: [] }),
						action: async () => ({ downloads: [] }),
						clear: async () => ({ downloads: [] }),
						onChanged: unsubscribe,
					},
					notifyPanelUsed: () => undefined,
					notifyPanelBlur: () => undefined,
					onFocusLocation: unsubscribe,
					onFindOpen: unsubscribe,
					onReopenClosedTab: unsubscribe,
					onClosePanel: unsubscribe,
					devtools: async (input: { viewId: string }) => ({ viewId: input.viewId, open: false, activeTabId: "" }),
					destroy: () => undefined,
					// Annotation contract (mirrors src/preload.ts): useBrowserView subscribes
					// to these whenever SessionView mounts with window.ao.browser present, so
					// an incomplete browser shape would crash the session-detail/preview specs.
					setAnnotationMode: async () => undefined,
					completeAnnotation: async () => undefined,
					discardAnnotations: async () => undefined,
					annotationAction: async () => undefined,
					onAnnotationSubmit: unsubscribe,
					onAnnotationCancel: unsubscribe,
					onAnnotationState: unsubscribe,
					onNavState: unsubscribe,
					onFindState: unsubscribe,
					onTabsState: unsubscribe,
					onAgentActivity: unsubscribe,
					onRuntimeState: unsubscribe,
					onDevToolsState: unsubscribe,
					onProfileState: unsubscribe,
					onProfileManage: unsubscribe,
					onPageFocus: unsubscribe,
				},
				browserProfiles: {
					list: async () => ({ profiles: [], defaultProfileId: null }),
					create: async (name: string) => {
						const now = new Date().toISOString();
						return { id: `fake-${name}`, name, createdAt: now, updatedAt: now };
					},
					rename: async ({ id, name }: { id: string; name: string }) => {
						const now = new Date().toISOString();
						return { id, name, createdAt: now, updatedAt: now };
					},
					clear: async () => undefined,
					usage: async () => ({ sessions: [] }),
					delete: async () => ({}),
					setDefault: async () => undefined,
					discoverImportSources: async () => ({ sources: [] }),
					import: async () => ({ sourceName: "", entries: [] }),
					onImportProgress: () => () => undefined,
				},
				notifications: {
					show: async () => undefined,
					setBadge: async (_count: number) => undefined,
					devBounce: async () => undefined,
					onClick: unsubscribe,
					onPlaySound: unsubscribe,
					reportSoundFailure: () => undefined,
				},
				tray: { setAttentionState: () => undefined, onOpenSession: unsubscribe },
				appState: { getMigration: async () => ({ status: "completed" }), setMigration: async () => undefined },
				cliLink: {
					setEnabled: async () => undefined,
					inspect: async () => ({ ok: false as const, code: "unsupported" as const, message: "unavailable" }),
					link: async () => ({ ok: false as const, code: "unsupported" as const, message: "unavailable" }),
					unlink: async () => ({ ok: false as const, code: "unsupported" as const, message: "unavailable" }),
				},
				updateSettings: {
					get: async () => ({
						enabled: false,
						channel: "latest",
						nightlyAck: false,
						feature: null,
						macDifferentialUpdates: false,
					}),
					set: async () => undefined,
					setMacDifferentialUpdates: async () => undefined,
				},
				uiSettings: {
					get: async () => ({ ...DEFAULT_UI_SETTINGS }),
					set: async (settings) => coerceUiSettings({ ...DEFAULT_UI_SETTINGS, ...settings }),
				},
				keybindings: {
					get: async () => ({}),
					set: async (overrides) => overrides,
					setRecording: async () => undefined,
				},
				updates: {
					getStatus: async () => ({ state: "idle" }),
					check: async () => undefined,
					returnHome: async () => undefined,
					download: async () => undefined,
					install: async () => undefined,
					isPostUpdateRelaunch: async () => false,
					relaunch: async () => undefined,
					onStatus: unsubscribe,
					onTelemetry: unsubscribe,
				},
				// UpdatesSection calls featureBuilds.getActive() immediately on mount; an
				// omitted namespace would surface as a swallowed React Query error.
				featureBuilds: {
					list: async () => [],
					getActive: async () => null,
				},
				remotes: {
					list: async () => [],
					importAccountHost: async () => undefined,
					pruneAccountHosts: async () => undefined,
					issueAccountToken: async () => "",
					add: async () => "offline" as const,
					update: async () => "offline" as const,
					remove: async () => undefined,
					connect: async (url: string) => {
						throw new Error(`No fake remote for ${url}`);
					},
					disconnect: async () => undefined,
					computerName: async () => "E2E computer",
					previewUrl: async (_hostId: string, _sessionId: string, sourceUrl: string) => sourceUrl,
					resolvePreviewUrl: async (_hostId: string, _sessionId: string, viewedUrl: string) => viewedUrl,
					agentBrowserUrl: async () => "",
					externalUrl: async (_hostId: string, url: string) => url,
				},
				cloud: {
					getSession: async () => null,
					signIn: async () => undefined,
					signOut: async () => undefined,
					cancelProviderAuth: async () => undefined,
					connectProviderAuth: async () => undefined,
					localAuthAvailable: async () => false,
					onOpenShareLink: () => () => undefined,
					takePendingShareLink: async () => null,
					localRegister: async () => {
						throw new Error("local auth is unavailable in e2e");
					},
					localLogin: async () => {
						throw new Error("local auth is unavailable in e2e");
					},
					onSessionChanged: unsubscribe,
				},
				cloudCp: {
					request: async () => ({ status: 401, headers: {}, body: "" }),
					openStream: async () => ({ streamId: "stream_test" }),
					closeStream: () => undefined,
					onStreamEvent: unsubscribe,
					agentBrowserUrl: async () => "",
				},
				daemonStream,
			} satisfies AoBridge;
			(window as unknown as { ao: unknown }).ao = ao;
		},
		{ version, daemonPort, projectId, projectName, platform, workers, nowIso, expandProject, nativeEvents },
	);
}
