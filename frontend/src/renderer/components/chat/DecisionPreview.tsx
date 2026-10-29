import { useTranslation } from "react-i18next";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { components } from "../../../api/schema";

// This renderer never invokes Chat's resource loaders, link previews or diagrams.
export function DecisionPreview({ preview, label }: { preview: components["schemas"]["AttentionPreview"]; label: string }) {
	const { t } = useTranslation();
	return <details className="mt-2 min-w-0 text-sm">
		<summary className="cursor-pointer break-words rounded-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring">{t("decision.preview", { label })}</summary>
		<div className="mt-2 min-w-0 break-words rounded-md bg-muted p-3 text-foreground">
			{preview.format === "text" ? <pre className="whitespace-pre-wrap break-words font-mono text-xs">{preview.content}</pre> :
				<ReactMarkdown skipHtml remarkPlugins={[remarkGfm]} components={{
					a: ({ children, href }) => <span>{children}{href ? <> ({href})</> : null}</span>,
					img: () => null,
					input: () => null,
					pre: ({ children }) => <pre className="max-w-full overflow-x-auto whitespace-pre-wrap break-words font-mono text-xs">{children}</pre>,
					code: ({ children }) => <code className="whitespace-pre-wrap break-words font-mono text-xs">{children}</code>,
					table: ({ children }) => <div className="max-w-full overflow-x-auto"><table className="w-full border-collapse text-sm">{children}</table></div>,
					th: ({ children }) => <th className="border border-border p-2 text-left font-medium">{children}</th>,
					td: ({ children }) => <td className="border border-border p-2">{children}</td>,
				}}>{preview.content}</ReactMarkdown>}
		</div>
	</details>;
}
