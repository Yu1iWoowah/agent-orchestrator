import { useIsMutating, useMutation, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { Play } from "lucide-react";
import { apiErrorMessage } from "../lib/api-client";
import { clientForSessionHost } from "../lib/host-clients";
import { workspaceQueryKeyForHost } from "../hooks/useWorkspaceQuery";
import { useCanResumeAgent } from "../hooks/useCanResumeAgent";
import { followResume, noteResumeFailure, useResumeWatch } from "../lib/resume-watch";
import { usesPreviewWorkspaceData as usePreviewData } from "../lib/preview-mode";
import { cn } from "../lib/utils";
import type { WorkspaceSession } from "../types/workspace";
import { Button } from "./ui/button";

/**
 * Relaunches an agent that exited inside a still-live session. Distinct from
 * restore, which revives a TERMINATED row: this keeps the worktree, terminal
 * identity, and native conversation. Self-gating, so the inspector Summary and
 * the terminal strip can both mount it unconditionally.
 */
export function ResumeAgentControl({
	className,
	containerClassName,
	session,
	hostId,
}: {
	className?: string;
	containerClassName?: string;
	session: WorkspaceSession;
	hostId?: string;
}) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const canResume = useCanResumeAgent(session, hostId);
	const watch = useResumeWatch(session.id, hostId);
	const resuming = useIsMutating({ mutationKey: ["resume-agent", hostId ?? "local", session.id] }) > 0 || watch?.state === "running";
	const resume = useMutation({
		mutationKey: ["resume-agent", hostId ?? "local", session.id],
		mutationFn: async () => {
			if (usePreviewData) return;
			const { data, error, response } = await clientForSessionHost(hostId).POST("/api/v1/sessions/{sessionId}/resume-agent", {
				params: { path: { sessionId: session.id } },
			});
			if (error) {
				noteResumeFailure(session.id, error, hostId);
				throw new Error(apiErrorMessage(error, `Failed to resume agent (${response.status})`));
			}
			// The daemon finishes the resume in the background; the watch shows
			// a failure here and refreshes the session once it settles.
			followResume(session.id, hostId, response, data, refresh);
		},
		onSuccess: refresh,
	});
	function refresh() {
		return queryClient.invalidateQueries({ queryKey: workspaceQueryKeyForHost(hostId) });
	}

	// Cloud sessions re-provision through the control plane (useRestoreSession),
	// not this local-daemon route — the local daemon has never heard of them and
	// would answer "Unknown session".
	if (!canResume) return null;

	const error = resume.error instanceof Error ? resume.error.message : watch?.state === "failed" ? watch.message : null;
	const control = (
		<>
			<Button
				className={cn("shrink-0", className)}
				disabled={resuming}
				onClick={() => resume.mutate()}
				size="sm"
				type="button"
				variant="outline"
			>
				<Play className="size-icon-sm" aria-hidden="true" />
				{t("inspector.resumeAgent")}
			</Button>
			{error ? (
				<p className="mt-2 text-xs leading-normal text-error" role="status">
					{error}
				</p>
			) : null}
		</>
	);
	return containerClassName ? <div className={containerClassName}>{control}</div> : control;
}
