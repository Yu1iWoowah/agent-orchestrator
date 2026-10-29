import type { AppLocale } from "../shared/ui-locale";
/** IDs come from the daemon lookup, never task names or caller-provided dialog text. */
export function appDiagnosticsDialog(locale: AppLocale, sessionId: string, projectId: string) {
	const zh = locale === "zh-TW";
	return {
		type: "question" as const,
		title: zh ? "允許本機 app 診斷？" : "Allow local app diagnostics?",
		message: zh
			? `允許 orchestrator ${sessionId} 擷取 app 結構快照 15 分鐘？`
			: `Allow orchestrator ${sessionId} to capture app structure for 15 minutes?`,
		detail: zh
			? `專案：${projectId}\n可多次讀取路由、畫面幾何、cache 與元件摘要。project／session ID 可能揭露名稱。不含對話內容、檔案內容或憑證；不外送。快照最多保存 24 小時／20 份／20 MiB，稽核保存 7 天／10,000 筆。你可隨時撤銷，並刪除這次授權的快照。相同本機帳號的程式仍可能讀取授權憑證。`
			: `Project: ${projectId}\nAllows repeated reads of route, layout geometry, cache and component summaries. Project/session IDs may reveal names. Excludes conversation and file contents and credentials; stays local. Snapshots: 24 hours / 20 files / 20 MiB. Audit: 7 days / 10,000 records. Revoke anytime to clear this grant's snapshots. Programs using your local account may still read its credential.`,
		buttons: zh ? ["取消", "允許 15 分鐘"] : ["Cancel", "Allow for 15 minutes"],
		defaultId: 0,
		cancelId: 0,
		noLink: true,
	};
}
