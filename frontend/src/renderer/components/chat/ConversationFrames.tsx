import { useTranslation } from "react-i18next";
import {
	type CSSProperties,
	type ReactNode,
	createContext,
	useContext,
	useCallback,
	useEffect,
	useId,
	useRef,
	useState,
} from "react";
import { createPortal } from "react-dom";
import { ChevronDown, ChevronUp, ArrowUpDown, RotateCcw } from "lucide-react";
import {
	defaultConversationFramePreferences,
	deriveConversationFrameLayout,
	frameLimits,
	type ConversationFrameKind,
	type FrameRect,
} from "../../lib/conversation-frames";
import {
	conversationFrameSessionKey,
	useConversationFrameStore,
} from "../../stores/conversation-frame-store";
import { useResizable } from "../../hooks/useResizable";
import { ResizeHandle } from "../ResizeHandle";
import { Button } from "../ui/button";
import { Sheet, SheetContent, SheetTitle } from "../ui/sheet";
import { Tooltip, TooltipContent, TooltipTrigger } from "../ui/tooltip";

export interface ConversationFrameDescriptor {
	kind: ConversationFrameKind;
	title: string;
	summary: string;
	content: ReactNode;
}
const defaults = defaultConversationFramePreferences();
const FrameNavigationContext = createContext<
	((navigate: () => void) => void) | undefined
>(undefined);
export const useConversationFrameNavigation = () =>
	useContext(FrameNavigationContext);

/** Inline header controls plus a non-layout overlay owned by the Chat surface. */
export function ConversationFrames({
	sessionId,
	hostId,
	surface,
	safeRect,
	frames,
}: {
	sessionId: string;
	hostId?: string;
	surface: HTMLElement;
	safeRect: FrameRect;
	frames: readonly ConversationFrameDescriptor[];
}) {
	const { t } = useTranslation();
	const key = conversationFrameSessionKey(sessionId, hostId);
	const preferences = useConversationFrameStore((state) => state.sessions[key] ?? defaults);
	const setOpen = useConversationFrameStore((state) => state.setFrameOpen);
	const [focusedFrame, setFocusedFrame] = useState<ConversationFrameKind>();
	const navigatingFromSheet = useRef(false);
	const entryRefs = useRef(new Map<ConversationFrameKind, HTMLButtonElement>());
	const headerRefs = useRef(new Map<ConversationFrameKind, HTMLButtonElement>());
	const id = useId();
	const [preview, setPreview] = useState<
		Partial<Record<ConversationFrameKind, { preferredWidth?: number; preferredHeight?: number }>>
	>({});
	const paintedPreferences = {
		plan: { ...preferences.plan, ...preview.plan },
		attention: { ...preferences.attention, ...preview.attention },
	};
	const layout = deriveConversationFrameLayout({
		safeRect,
		preferences: paintedPreferences,
		availableFrames: frames.map((frame) => frame.kind),
	});
	useEffect(() => {
		setFocusedFrame(undefined);
		setPreview({});
	}, [key]);
	const tooSmall = safeRect.width < 100 || safeRect.height < 40;
	const open = (kind: ConversationFrameKind) => {
		if (tooSmall) return;
		surface.dispatchEvent(new CustomEvent("ao:frame-foreground"));
		if (layout.compact) setFocusedFrame(kind);
		else {
			setOpen(key, kind, true);
			requestAnimationFrame(() => document.getElementById(`${id}-${kind}`)?.focus());
		}
	};
	const close = (kind: ConversationFrameKind) => {
		setFocusedFrame(undefined);
		if (!layout.compact && focusedFrame !== kind) setOpen(key, kind, false);
		if (!navigatingFromSheet.current)
			requestAnimationFrame(() =>
				(entryRefs.current.get(kind) ?? headerRefs.current.get(kind))?.focus(),
			);
	};
	return (
		<FrameNavigationContext.Provider
			value={(navigate) => {
				if (focusedFrame) {
					navigatingFromSheet.current = true;
					setFocusedFrame(undefined);
				}
				navigate();
			}}
		>
			{frames.map((frame) => (
				<Tooltip key={frame.kind}>
					<TooltipTrigger asChild>
						<Button
							ref={(node) => {
								if (node) headerRefs.current.set(frame.kind, node);
								else headerRefs.current.delete(frame.kind);
							}}
							variant="ghost"
							size="sm"
							aria-label={t("frames.open", { title: frame.title })}
							aria-disabled={tooSmall || undefined}
							aria-expanded={Boolean(
								layout.frames[frame.kind]?.open || focusedFrame === frame.kind,
							)}
							aria-controls={`${id}-${frame.kind}`}
							onClick={() => open(frame.kind)}
						>
							{frame.title}
						</Button>
					</TooltipTrigger>
					<TooltipContent>
						{tooSmall ? t("frames.needSpace") : `${frame.title} ${frame.summary}`}
					</TooltipContent>
				</Tooltip>
			))}
			{createPortal(
				<div
					className="pointer-events-none absolute inset-0 z-20"
					data-conversation-frames=""
					onClick={(event) => event.stopPropagation()}
				>
					{frames.map((frame) => {
						const placement = layout.frames[frame.kind];
						if (!placement || layout.headerOnly || focusedFrame === frame.kind) return null;
						return placement.open ? (
							<FrameChrome
								key={frame.kind}
								id={`${id}-${frame.kind}`}
								frame={frame}
								rect={placement.rect}
								safeRect={safeRect}
								stateKey={key}
								onClose={() => close(frame.kind)}
								canSwap={frames.length > 1}
								maxHeight={
									safeRect.height -
									frames
										.filter((other) => other.kind !== frame.kind)
										.reduce(
											(sum, other) =>
												sum +
												(preferences[other.kind].userOpen
													? frameLimits[other.kind].minHeight
													: 32) +
												12,
											0,
										)
								}
								onPreview={(axis, value) =>
									setPreview((current) => {
										const next = { ...current[frame.kind] };
										const property = axis === "x" ? "preferredWidth" : "preferredHeight";
										if (value === null) delete next[property];
										else next[property] = value;
										return { ...current, [frame.kind]: next };
									})
								}
							/>
						) : (
							<Button
								key={frame.kind}
								ref={(node) => {
									if (node) entryRefs.current.set(frame.kind, node);
									else entryRefs.current.delete(frame.kind);
								}}
								variant="outline"
								size="sm"
								className="pointer-events-auto absolute h-8 justify-between rounded-lg bg-card shadow-sm"
								style={{ ...placement.rect }}
								aria-expanded={false}
								aria-controls={`${id}-${frame.kind}`}
								onClick={() => open(frame.kind)}
							>
								{frame.title} {frame.summary}
								<ChevronDown className="size-3" aria-hidden="true" />
							</Button>
						);
					})}
				</div>,
				surface,
			)}
			{frames.map((frame) => (
				<Sheet
					key={frame.kind}
					modal={false}
					open={focusedFrame === frame.kind}
					onOpenChange={(value) => {
						if (!value) close(frame.kind);
					}}
				>
					<SheetContent
						showCloseButton={false}
						aria-describedby={undefined}
						style={{
							inset: "auto",
							left:
								surface.getBoundingClientRect().left +
								safeRect.left +
								safeRect.width -
								Math.min(safeRect.width, frameLimits[frame.kind].maxWidth),
							top: surface.getBoundingClientRect().top + safeRect.top,
							width: safeRect.width,
							height: Math.min(safeRect.height, frameLimits[frame.kind].maxHeight),
							maxWidth: frameLimits[frame.kind].maxWidth,
						}}
						className="gap-0 overflow-hidden rounded-xl border bg-card"
						onCloseAutoFocus={(event) => {
							event.preventDefault();
							if (!navigatingFromSheet.current)
								headerRefs.current.get(frame.kind)?.focus();
						navigatingFromSheet.current = false;
						}}
					>
						<div className="flex h-10 shrink-0 items-center justify-between border-b px-3">
							<SheetTitle className="text-sm">{frame.title}</SheetTitle>
							<Button
								variant="ghost"
								size="sm"
								onClick={() => close(frame.kind)}
								aria-label={t("frames.collapse", { title: frame.title })}
							>
								<ChevronUp className="size-4" />
							</Button>
						</div>
						<div className="min-h-0 flex-1 overflow-auto p-3">{frame.content}</div>
					</SheetContent>
				</Sheet>
			))}
		</FrameNavigationContext.Provider>
	);
}

function FrameChrome(props: {
	id:string; frame:ConversationFrameDescriptor;rect:FrameRect;safeRect:FrameRect;stateKey:string;onClose:()=>void;canSwap:boolean;maxHeight:number;onPreview:(axis:"x"|"y",value:number|null)=>void;
}) {
	const size=useConversationFrameStore(state=>state.setFrameSize);
	const swap=useConversationFrameStore(state=>state.swapFrameSlots);
	return <ResizableConversationFrame {...props} limits={frameLimits[props.frame.kind]} onSize={value=>size(props.stateKey,props.frame.kind,value)} onSwap={props.canSwap?()=>swap(props.stateKey):undefined}/>;
}

export function ResizableConversationFrame({
	id,
	frame,
	rect,
	safeRect,
	limits,
	onSize,
	onSwap,
	onClose,
	maxHeight,
	onPreview,
	scrollContent = true,
}: {
	id: string;
	frame: Pick<ConversationFrameDescriptor, "title" | "summary" | "content">;
	rect: FrameRect;
	safeRect: FrameRect;
	limits: typeof frameLimits.plan;
	onSize: (size: {width?:number;height?:number}) => void;
	onSwap?: () => void;
	onClose: () => void;
	maxHeight: number;
	onPreview: (axis: "x" | "y", value: number | null) => void;
	scrollContent?: boolean;
}) {
	const { t } = useTranslation();
	const ref = useRef<HTMLElement>(null);
	const initial = useRef({ width: rect.width, height: rect.height });
	const maxHeightPx = Math.min(
		limits.maxHeight,
		maxHeight,
		safeRect.top + safeRect.height - rect.top,
	);
	const liveMax = useRef({
		width: Math.min(limits.maxWidth, safeRect.width),
		height: maxHeightPx,
	});
	liveMax.current = {
		width: Math.min(limits.maxWidth, safeRect.width),
		height: maxHeightPx,
	};
	const maxWidthValue = useCallback(() => liveMax.current.width, []);
	const maxHeightValue = useCallback(() => liveMax.current.height, []);
	const targets = useCallback(() => [ref.current], []);
	const width = useResizable({
		cssVar: "--frame-width",
		getCssTargets: targets,
		defaultWidth: initial.current.width,
		value: rect.width,
		resetSize: limits.width,
		cancelOnInterrupt: true,
		onPreview: (value) => onPreview("x", value),
		min: limits.minWidth,
		max: maxWidthValue,
		edge: "left",
		onCommit: (value) => onSize({ width: value }),
	});
	const height = useResizable({
		cssVar: "--frame-height",
		getCssTargets: targets,
		defaultWidth: initial.current.height,
		value: rect.height,
		resetSize: limits.height,
		cancelOnInterrupt: true,
		onPreview: (value) => onPreview("y", value),
		min: limits.minHeight,
		max: maxHeightValue,
		edge: "right",
		axis: "y",
		onCommit: (value) => onSize({ height: value }),
	});
	const style: CSSProperties = {
		top: rect.top,
		right: `calc(100% - ${safeRect.left + safeRect.width}px)`,
		width: "var(--frame-width)",
		height: "var(--frame-height)",
		maxWidth: safeRect.width,
		maxHeight: safeRect.top + safeRect.height - rect.top,
	};
	return (
		<section
			ref={ref}
			id={id}
			aria-label={frame.title}
			role="region"
			tabIndex={-1}
			className="pointer-events-auto absolute flex flex-col rounded-xl border border-border bg-card text-card-foreground shadow-lg"
			style={style}
			onKeyDown={(event) => {
				if (event.key === "Escape" && !event.defaultPrevented) {
					event.stopPropagation();
					onClose();
				}
			}}
		>
			<div className="flex h-10 shrink-0 items-center gap-2 border-b px-3">
				<span className="min-w-0 flex-1 truncate text-sm font-medium">{frame.title}</span>
				<span className="text-xs tabular-nums text-muted-foreground">{frame.summary}</span>
				{onSwap ? (
					<Button
						variant="ghost"
						size="icon-sm"
						aria-label={t("frames.swap")}
						onClick={onSwap}
					>
						<ArrowUpDown className="size-3.5" />
					</Button>
				) : null}
				<Button
					variant="ghost"
					size="icon-sm"
					aria-label={t("frames.reset")}
					onClick={() => onSize({ width: limits.width, height: limits.height })}
				>
					<RotateCcw className="size-3.5" />
				</Button>
				<Button
					variant="ghost"
					size="icon-sm"
					aria-label={t("frames.collapse", { title: frame.title })}
					onClick={onClose}
				>
					<ChevronUp className="size-4" />
				</Button>
			</div>
			<div className={scrollContent ? "min-h-0 flex-1 overflow-auto overscroll-contain p-3" : "flex min-h-0 flex-1 flex-col overflow-hidden p-3"}>{frame.content}</div>
			<ResizeHandle
				side="left"
				tabIndex={0}
				role="separator"
				aria-label={t("frames.resizeWidth", { title: frame.title })}
				aria-orientation="vertical"
				aria-valuenow={rect.width}
				aria-valuemin={limits.minWidth}
				aria-valuemax={Math.min(limits.maxWidth, safeRect.width)}
				onPointerDown={width.onPointerDown}
				onKeyDown={width.onKeyDown}
				onDoubleClick={width.onDoubleClick}
			/>
			<div
				tabIndex={0}
				role="separator"
				aria-label={t("frames.resizeHeight", { title: frame.title })}
				aria-orientation="horizontal"
				aria-valuenow={rect.height}
				aria-valuemin={limits.minHeight}
				aria-valuemax={maxHeightPx}
				className="absolute inset-x-2 -bottom-1 h-2 cursor-row-resize touch-none"
				onPointerDown={height.onPointerDown}
				onKeyDown={height.onKeyDown}
				onDoubleClick={height.onDoubleClick}
			/>
		</section>
	);
}
