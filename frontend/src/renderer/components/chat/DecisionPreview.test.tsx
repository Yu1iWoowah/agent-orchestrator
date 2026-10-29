import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { DecisionPreview } from "./DecisionPreview";

describe("decision preview renderer resource boundary", () => {
	it("does not create resources, raw HTML or navigation even when validation is bypassed", () => {
		const { container } = render(<DecisionPreview label="Untrusted" preview={{ format: "markdown", content: [
			'<iframe src="https://example.com/private"></iframe>',
			'<img src="file:///private/file">',
			'![remote](https://example.com/pixel.png)',
			'[Run](javascript:alert(1))',
			'[Reference](https://example.com)',
			'```html\n<script>alert(1)</script>\n```',
		].join("\n\n") }} />);
		expect(container.querySelectorAll("img,iframe,script,video,audio,object,embed,a[href],link,input")).toHaveLength(0);
		expect(screen.getByText("<script>alert(1)</script>")).toBeInTheDocument();
		expect(container.textContent).toContain("Reference");
	});

	it("keeps text literal and preview disclosure independent of question choices", () => {
		const { container } = render(<DecisionPreview label="Literal" preview={{ format: "text", content: '<img src="https://example.com/private">' }} />);
		expect(container.querySelectorAll("img,input,button")).toHaveLength(0);
		expect(container.querySelector("details")).not.toHaveAttribute("open");
		expect(container.querySelector("pre")).toHaveTextContent('<img src="https://example.com/private">');
	});
});
