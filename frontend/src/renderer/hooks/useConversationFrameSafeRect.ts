import { type RefObject, useEffect, useState } from "react";
import { conversationFrameSafeRect, type FrameRect } from "../lib/conversation-frames";

export function useConversationFrameSafeRect(
	surfaceRef: RefObject<HTMLElement | null>,
	composerRef?: RefObject<HTMLElement | null>,
) {
	const [bounds, setBounds] = useState<{
		surface: HTMLElement;
		rect: FrameRect;
	}>();
	useEffect(() => {
		const surface = surfaceRef.current;
		const composer = composerRef?.current;
		if (!surface || (composerRef && !composer)) return;
		let frame: number | undefined;
		const measure = () => {
			const viewport = surface.querySelector<HTMLElement>(".chat-scroll-viewport");
			const header = surface.querySelector<HTMLElement>("header");
			const box = surface.getBoundingClientRect();
			const headerBottom = header?.getBoundingClientRect().bottom ?? box.top;
			const rect = conversationFrameSafeRect({
				surface: box,
				viewport: viewport?.getBoundingClientRect() ?? {
					left: box.left,
					top: headerBottom,
					width: box.width,
					height: box.bottom - headerBottom,
				},
				composer: composer?.getBoundingClientRect() ?? { left: box.left, top: box.bottom, width: box.width, height: 0 },
				visible: {
					left: window.visualViewport?.offsetLeft ?? 0,
					top: window.visualViewport?.offsetTop ?? 0,
					width: window.visualViewport?.width ?? window.innerWidth,
					height: window.visualViewport?.height ?? window.innerHeight,
				},
				avoid: Array.from(
					surface.querySelectorAll<HTMLElement>("[data-conversation-frame-avoid]"),
				).map((toolbar) => toolbar.getBoundingClientRect()),
			});
			setBounds((old) =>
				old?.surface === surface &&
				old.rect.left === rect.left &&
				old.rect.top === rect.top &&
				old.rect.width === rect.width &&
				old.rect.height === rect.height
					? old
					: { surface, rect },
			);
		};
		const schedule = () => {
			if (frame === undefined)
				frame = requestAnimationFrame(() => {
					frame = undefined;
					measure();
				});
		};
		const observer = new ResizeObserver(schedule);
		observer.observe(surface);
		if (composer) observer.observe(composer);
		const viewport = surface.querySelector<HTMLElement>(".chat-scroll-viewport");
		if (viewport) observer.observe(viewport);
		const mutations = new MutationObserver(schedule);
		if (viewport) {
			mutations.observe(viewport, { childList: true, subtree: true });
			viewport.addEventListener("scroll", schedule, { passive: true });
		}
		if (composer) mutations.observe(composer, {
			childList: true,
			subtree: true,
			attributes: true,
		});
		window.addEventListener("resize", schedule);
		window.visualViewport?.addEventListener("resize", schedule);
		window.visualViewport?.addEventListener("scroll", schedule);
		measure();
		return () => {
			observer.disconnect();
			mutations.disconnect();
			viewport?.removeEventListener("scroll", schedule);
			window.removeEventListener("resize", schedule);
			window.visualViewport?.removeEventListener("resize", schedule);
			window.visualViewport?.removeEventListener("scroll", schedule);
			if (frame !== undefined) cancelAnimationFrame(frame);
		};
	}, [surfaceRef, composerRef]);
	return bounds;
}
