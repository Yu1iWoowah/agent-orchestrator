import { useCallback, useEffect, useLayoutEffect, useRef } from "react";

type ResizableConstraint = number | (() => number);

interface UseResizableOptions {
	/** CSS custom property to drive, e.g. "--ao-sidebar-w". */
	cssVar: string;
	/**
	 * Limits custom-property invalidation to the elements that consume the
	 * width. Defaults to :root for callers that do not provide local targets.
	 */
	getCssTargets?: () => ReadonlyArray<HTMLElement | null>;
	/** localStorage key to persist the width. */
	storageKey?: string;
	/** Controlled frames persist through their owner instead of a second storage key. */
	onCommit?: (size: number) => void;
	axis?: "x" | "y";
	value?: number;
	resetSize?: number;
	onPreview?: (size: number | null) => void;
	defaultWidth: number;
	min: ResizableConstraint;
	max: ResizableConstraint;
	/**
	 * Which edge the drag handle sits on relative to the panel it resizes.
	 * "right" (sidebar handle) grows with rightward drag; "left" (inspector
	 * handle) grows with leftward drag.
	 */
	edge: "left" | "right";
	/** Called once when a collapsed rail drag should reopen the owner. */
	onExpand?: () => void;
	/** Optional one-time floor when restoring a saved width for a new panel profile. */
	restoreMin?: number;
	/**
	 * Grow a width clamped by a live `max` (a restore or reset in a narrow window
	 * or zoom) back toward the preferred width when the window resizes. Grow
	 * only: narrowing leaves the var to the consumer's CSS max-width, so a later
	 * split widening (e.g. collapsing a sidebar) still recovers. Only window
	 * resizes count: a sibling panel's spring must not resize this one per frame.
	 */
	reclampOnWindowResize?: boolean;
	/** Pointer movement needed before a collapsed rail drag expands. */
	expandDragThreshold?: number;
}

/**
 * Pointer-driven panel resize, cloned from agent-orchestrator's useResizable.
 * Persists the width to localStorage and applies it via a CSS custom property
 * to the nearest consuming layout elements. Keeping a high-frequency custom
 * property off :root avoids invalidating unrelated renderer subtrees.
 *
 * Single owner of clamped width (do not regress):
 * - `apply` is write-only: clamp with `min`/`max` only. Callers (esp. inspector)
 *   must pass a live `max` callback — never cache a loose placeholder like
 *   `defaultWidth * 2` as the drag ceiling. The unclamped preference is kept
 *   separately so a window resize can grow a narrow-clamped width back.
 * - On pointerdown, seed the drag from the painted box when it disagrees with
 *   the var (CSS max-width can hold paint below the custom property). Nothing is
 *   written or persisted until the width actually changes.
 * - Drag applies once per frame.
 * - Dragging never auto-collapses: clamp at `min`; collapse stays on explicit UI.
 */
export function useResizable({
	cssVar,
	getCssTargets,
	storageKey,
	defaultWidth,
	min,
	max,
	edge,
	onExpand,
	restoreMin,
	reclampOnWindowResize = false,
	expandDragThreshold = 8,
	onCommit,
	axis = "x",
	value,
	resetSize,
	onPreview,
}: UseResizableOptions) {
	const previewRef = useRef(onPreview);
	previewRef.current = onPreview;
	const commitRef = useRef(onCommit);
	commitRef.current = onCommit;
	const persist = useCallback((size: number) => {
		if (storageKey) window.localStorage.setItem(storageKey, String(size));
		commitRef.current?.(size);
	}, [storageKey]);
	const widthRef = useRef(defaultWidth);
	// The width the user asked for, before the live clamp.
	const preferredWidthRef = useRef(defaultWidth);
	const frameRef = useRef<number | null>(null);
	const pendingWidthRef = useRef<number | null>(null);
	const appliedTargetsRef = useRef(new Set<HTMLElement>());
	const activeDragCleanupRef = useRef<(() => void) | null>(null);
	const minValue = useCallback(() => (typeof min === "function" ? min() : min), [min]);
	const maxValue = useCallback(() => (typeof max === "function" ? max() : max), [max]);
	const cssTargets = useCallback(
		() =>
			getCssTargets
				? getCssTargets().filter((target): target is HTMLElement => target !== null)
				: [document.documentElement],
		[getCssTargets],
	);

	const apply = useCallback(
		(next: number) => {
			const clamped = Math.min(maxValue(), Math.max(minValue(), next));
			widthRef.current = clamped;
			const targets = cssTargets();
			for (const target of targets) {
				if (target.style.getPropertyValue(cssVar) !== `${clamped}px`) {
					target.style.setProperty(cssVar, `${clamped}px`);
			}
				appliedTargetsRef.current.add(target);
			}
		},
		[cssTargets, cssVar, maxValue, minValue],
	);

	const applyOnFrame = useCallback(
		(next: number) => {
			pendingWidthRef.current = next;
			if (frameRef.current !== null) return;
			frameRef.current = window.requestAnimationFrame(() => {
				frameRef.current = null;
				const pending = pendingWidthRef.current;
				pendingWidthRef.current = null;
				if (pending !== null) { apply(pending); previewRef.current?.(widthRef.current); }
			});
		},
		[apply],
	);

	const flushPending = useCallback(() => {
		if (frameRef.current !== null) {
			window.cancelAnimationFrame(frameRef.current);
			frameRef.current = null;
		}
		const pending = pendingWidthRef.current;
		pendingWidthRef.current = null;
		if (pending !== null) apply(pending);
	}, [apply]);

	// Restore persisted width before first paint so the sidebar does not appear
	// at the default width on reload and then jump/animate to the stored width.
	useLayoutEffect(() => {
		const saved = Number(storageKey ? window.localStorage.getItem(storageKey) : defaultWidth);
		const restored = Number.isFinite(saved) && saved > 0 ? saved : defaultWidth;
		preferredWidthRef.current = restoreMin === undefined ? restored : Math.max(restoreMin, restored);
		apply(preferredWidthRef.current);
		return () => {
			activeDragCleanupRef.current?.();
			if (frameRef.current !== null) window.cancelAnimationFrame(frameRef.current);
			for (const target of appliedTargetsRef.current) target.style.removeProperty(cssVar);
			appliedTargetsRef.current.clear();
		};
	}, [apply, cssVar, defaultWidth, restoreMin, storageKey]);

	useLayoutEffect(() => { if (value !== undefined && !activeDragCleanupRef.current) apply(value); }, [value, apply]);

	useEffect(() => {
		if (!reclampOnWindowResize) return;
		let frame: number | null = null;
		const onResize = () => {
			if (frame !== null) return;
			frame = window.requestAnimationFrame(() => {
				frame = null;
				// A drag owns the width until it ends.
				if (activeDragCleanupRef.current) return;
				const next = Math.min(maxValue(), Math.max(minValue(), preferredWidthRef.current));
				if (next > widthRef.current + 0.5) apply(next);
			});
		};
		window.addEventListener("resize", onResize);
		return () => {
			window.removeEventListener("resize", onResize);
			if (frame !== null) window.cancelAnimationFrame(frame);
		};
	}, [apply, maxValue, minValue, reclampOnWindowResize]);

	const onPointerDown = useCallback(
		(event: React.PointerEvent<HTMLElement>) => {
			activeDragCleanupRef.current?.();
			event.preventDefault();
			const pointerId = event.pointerId;
			const captureTarget = event.currentTarget;
			captureTarget.setPointerCapture?.(pointerId);
			const startX = axis === "y" ? event.clientY : event.clientX;
			// Seed from the painted box when CSS max-width holds width below the var.
			const visualWidth = cssTargets()
				.map((target) => axis === "y" ? target.getBoundingClientRect().height : target.getBoundingClientRect().width)
				.find((width) => width > 0);
			const seedWidth =
				visualWidth !== undefined && Math.abs(visualWidth - widthRef.current) > 0.5 ? visualWidth : widthRef.current;
			const startWidth = Math.min(maxValue(), Math.max(minValue(), seedWidth));
			const sign = edge === "right" ? 1 : -1;
			// Nothing is written or persisted until the width actually changes: a
			// press, or a drag in a window too narrow to resize, must not pin the
			// panel at its narrow-window width.
			let changed = false;
			document.body.classList.add(axis === "y" ? "is-resizing-y" : "is-resizing-x");

			const finish = (cancelled = false) => {
				window.removeEventListener("keydown", onKey, true);
				window.removeEventListener("pointermove", onMove);
				window.removeEventListener("pointerup", onEnd);
				window.removeEventListener("pointercancel", onEnd);
				window.removeEventListener("blur", onBlur);
				flushPending();
				document.body.classList.remove(axis === "y" ? "is-resizing-y" : "is-resizing-x");
				if (captureTarget.hasPointerCapture?.(pointerId)) captureTarget.releasePointerCapture(pointerId);
				if (cancelled) apply(startWidth);
				if (changed && !cancelled) {
					preferredWidthRef.current = widthRef.current;
					persist(widthRef.current);
				}
				activeDragCleanupRef.current = null;
				previewRef.current?.(null);
			};
			const onEnd = (e: PointerEvent) => {
				if (e.pointerId === pointerId) finish(e.type === "pointercancel");
			};
			const onKey = (e: KeyboardEvent) => { if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); finish(true); } };
			window.addEventListener("keydown", onKey, true);
			// Keep only the newest pointer position for the next paint.
			// Collapse stays on explicit controls — `apply` clamps at `min`.
			const onMove = (e: PointerEvent) => {
				if (e.pointerId !== pointerId) return;
				const next = Math.min(maxValue(), Math.max(minValue(), startWidth + sign * ((axis === "y" ? e.clientY : e.clientX) - startX)));
				if (!changed && Math.abs(next - startWidth) < 0.5) return;
				changed = true;
				applyOnFrame(next);
			};
			window.addEventListener("pointermove", onMove);
			window.addEventListener("pointerup", onEnd);
			window.addEventListener("pointercancel", onEnd);
			const onBlur = () => finish(true);
			window.addEventListener("blur", onBlur);
			activeDragCleanupRef.current = () => finish(true);
		},
		[apply, applyOnFrame, cssTargets, edge, flushPending, maxValue, minValue, persist, axis],
	);

	const onCollapsedPointerDown = useCallback(
		(event: React.PointerEvent<HTMLElement>) => {
			activeDragCleanupRef.current?.();
			const pointerId = event.pointerId;
			const captureTarget = event.currentTarget;
			captureTarget.setPointerCapture?.(pointerId);
			const startX = axis === "y" ? event.clientY : event.clientX;
			const sign = edge === "right" ? 1 : -1;
			let expanded = false;
			document.body.classList.add(axis === "y" ? "is-resizing-y" : "is-resizing-x");

			const finish = () => {
				window.removeEventListener("pointermove", onMove);
				window.removeEventListener("pointerup", onEnd);
				window.removeEventListener("pointercancel", onEnd);
				window.removeEventListener("blur", finish);
				flushPending();
				document.body.classList.remove(axis === "y" ? "is-resizing-y" : "is-resizing-x");
				if (captureTarget.hasPointerCapture?.(pointerId)) captureTarget.releasePointerCapture(pointerId);
				if (expanded) {
					preferredWidthRef.current = widthRef.current;
					persist(widthRef.current);
				}
				if (activeDragCleanupRef.current === finish) activeDragCleanupRef.current = null;
			};
			const onEnd = (e: PointerEvent) => {
				if (e.pointerId === pointerId) finish();
			};
			const onMove = (e: PointerEvent) => {
				if (e.pointerId !== pointerId) return;
				const delta = sign * ((axis === "y" ? e.clientY : e.clientX) - startX);
				if (delta < expandDragThreshold) return;
				if (!expanded) {
					expanded = true;
					onExpand?.();
				}
				applyOnFrame(minValue() + delta);
			};
			window.addEventListener("pointermove", onMove);
			window.addEventListener("pointerup", onEnd);
			window.addEventListener("pointercancel", onEnd);
			window.addEventListener("blur", finish);
			activeDragCleanupRef.current = finish;
		},
		[applyOnFrame, edge, expandDragThreshold, flushPending, minValue, onExpand, persist, axis],
	);

	/** Double-click the handle to reset to the default width. */
	const onDoubleClick = useCallback(() => {
		preferredWidthRef.current = resetSize ?? defaultWidth;
		apply(resetSize ?? defaultWidth);
		persist(resetSize ?? defaultWidth);
	}, [apply, defaultWidth, persist, resetSize]);

	const onKeyDown = useCallback((event: React.KeyboardEvent<HTMLElement>) => {
		const smaller = axis === "y" ? "ArrowUp" : edge === "left" ? "ArrowRight" : "ArrowLeft";
		const larger = axis === "y" ? "ArrowDown" : edge === "left" ? "ArrowLeft" : "ArrowRight";
		if (event.key !== smaller && event.key !== larger) return;
		event.preventDefault();
		apply(widthRef.current + (event.key === larger ? 16 : -16));
		preferredWidthRef.current = widthRef.current;
		persist(widthRef.current);
	}, [apply, axis, edge, persist]);
	return { onPointerDown, onCollapsedPointerDown, onDoubleClick, onKeyDown };
}
