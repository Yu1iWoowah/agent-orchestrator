import { FocusScope } from "@radix-ui/react-focus-scope";
import { render } from "@testing-library/react";
import { afterAll, describe, expect, it, vi } from "vitest";

describe("DOM teardown with real timers", () => {
	const onUnmount = vi.fn();

	it("leaves a mounted focus scope for the shared cleanup", () => {
		expect(vi.isFakeTimers()).toBe(false);
		render(<FocusScope onUnmountAutoFocus={onUnmount}><button>Focus target</button></FocusScope>);
	});

	it("finishes deferred focus restoration before the next test", () => {
		expect(document.body).toBeEmptyDOMElement();
		expect(onUnmount).toHaveBeenCalledOnce();
		expect(onUnmount.mock.calls[0][0]).toBeInstanceOf(window.CustomEvent);
		expect(vi.isFakeTimers()).toBe(false);
	});
});

describe("DOM teardown with fake timers", () => {
	const onUnmount = vi.fn();
	const later = vi.fn();

	afterAll(() => {
		vi.clearAllTimers();
		vi.useRealTimers();
	});

	it("leaves a mounted focus scope and a future timer", () => {
		vi.useFakeTimers();
		setTimeout(later, 1_000);
		render(<FocusScope onUnmountAutoFocus={onUnmount}><button>Focus target</button></FocusScope>);
	});

	it("finishes focus restoration without changing the clock or running future work", () => {
		expect(document.body).toBeEmptyDOMElement();
		expect(onUnmount).toHaveBeenCalledOnce();
		expect(onUnmount.mock.calls[0][0]).toBeInstanceOf(window.CustomEvent);
		expect(vi.isFakeTimers()).toBe(true);
		expect(later).not.toHaveBeenCalled();
		vi.advanceTimersByTime(1_000);
		expect(later).toHaveBeenCalledOnce();
	});
});
