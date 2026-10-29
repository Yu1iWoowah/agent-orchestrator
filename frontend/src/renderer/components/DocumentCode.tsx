import { useMemo, type CSSProperties } from "react";
import { File } from "@pierre/diffs/react";
import { useTranslation } from "react-i18next";
import { useUiStore } from "../stores/ui-store";
import { writeDocumentText } from "../lib/document-clipboard";
import { AO_PIERRE_SURFACE_CSS, PIERRE_HIGHLIGHTER } from "./diffs/pierreTheme";
import { Button } from "./ui/button";
/** A read-only file surface without workspace, editing, annotation or Git dependencies. */
export function DocumentCode({
	text,
	path,
	zoom,
}: {
	text: string;
	path: string;
	zoom: number;
}) {
	const { t } = useTranslation();
	const theme = useUiStore((state) => state.resolvedTheme);
	const file = useMemo(
		() => ({
			name: path,
			contents: text,
			...(text.length > 100_000 ? { lang: "text" as const } : {}),
		}),
		[text, path],
	);
	const copy = async () => {
		try {
			await writeDocumentText(text);
			useUiStore.getState().showGlobalToast(t("documents.sheet.copied"));
		} catch {
			useUiStore
				.getState()
				.showGlobalToast(t("documents.sheet.copyError"), undefined, {
					tone: "error",
				});
		}
	};
	const style: CSSProperties & { "--diffs-font-size": string } = {
		"--diffs-font-size": `${13 * zoom}px`,
	};
	return (
		<div style={style}>
			<Button variant="ghost" size="sm" onClick={() => void copy()}>
				{t("documents.sheet.copyContents")}
			</Button>
			<File
				file={file}
				edit={false}
				disableWorkerPool={typeof Worker === "undefined"}
				options={{
					disableFileHeader: true,
					disableLineNumbers: false,
					overflow: "wrap",
					theme: { dark: "github-dark", light: "github-light" },
					themeType: theme,
					preferredHighlighter: PIERRE_HIGHLIGHTER,
					tokenizeMaxLength: 100_000,
					tokenizeMaxLineLength: 20_000,
					unsafeCSS: AO_PIERRE_SURFACE_CSS,
				}}
			/>
		</div>
	);
}
