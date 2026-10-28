import { FocusScope } from "radix-ui/internal";
import { render } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

describe.each(["real", "fake"])("DOM teardown with %s timers", (mode) => {
	it("finishes deferred focus restoration after shared cleanup", ({ onTestFinished }) => {
		const onUnmount = vi.fn();
		if (mode === "fake") vi.useFakeTimers();
		onTestFinished(() => {
			try {
				expect(document.body).toBeEmptyDOMElement();
				expect(onUnmount).toHaveBeenCalledOnce();
				expect(onUnmount.mock.calls[0][0]).toBeInstanceOf(window.CustomEvent);
				expect(vi.isFakeTimers()).toBe(mode === "fake");
			} finally {
				if (vi.isFakeTimers()) {
					vi.clearAllTimers();
					vi.useRealTimers();
				}
			}
		});
		render(<FocusScope.Root onUnmountAutoFocus={onUnmount}><button>Focus target</button></FocusScope.Root>);
	});

	it("preserves pending work after explicit unmount", ({ onTestFinished }) => {
		const onUnmount = vi.fn();
		const later = vi.fn();
		if (mode === "fake") vi.useFakeTimers();
		const scheduledAt = Date.now();
		const timer = setTimeout(later, 60_000);
		onTestFinished(() => {
			try {
				expect(document.body).toBeEmptyDOMElement();
				expect(onUnmount).toHaveBeenCalledOnce();
				expect(onUnmount.mock.calls[0][0]).toBeInstanceOf(window.CustomEvent);
				expect(vi.isFakeTimers()).toBe(mode === "fake");
				expect(later).not.toHaveBeenCalled();
				if (mode === "fake") {
					expect(Date.now()).toBe(scheduledAt);
					vi.advanceTimersByTime(60_000);
					expect(later).toHaveBeenCalledOnce();
				}
			} finally {
				clearTimeout(timer);
				if (vi.isFakeTimers()) {
					vi.clearAllTimers();
					vi.useRealTimers();
				}
			}
		});
		const view = render(<FocusScope.Root onUnmountAutoFocus={onUnmount}><button>Focus target</button></FocusScope.Root>);
		view.unmount();
	});
});
