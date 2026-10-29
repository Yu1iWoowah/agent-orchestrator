import * as React from "react";
import { cn } from "../../lib/utils";

export const Textarea = React.forwardRef<HTMLTextAreaElement, React.TextareaHTMLAttributes<HTMLTextAreaElement>>(
	({ className, ...props }, ref) => (
		<textarea
			data-slot="textarea"
			ref={ref}
			className={cn("min-h-20 w-full min-w-0 rounded-md border border-border bg-input/50 px-3 py-2 text-sm text-foreground outline-none placeholder:text-muted-foreground focus-visible:ring-2 focus-visible:ring-ring aria-invalid:border-destructive disabled:cursor-not-allowed", className)}
			{...props}
		/>
	),
);
Textarea.displayName = "Textarea";
