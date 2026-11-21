import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { expect, it, vi } from "vitest";
import type { DisplayRow } from "./model";

vi.mock("react-native", async () => {
	const { createElement } = await import("react");
	const host = (name: string) => ({ children, ...props }: { children?: React.ReactNode }) => createElement(name, props, children);
	return {
		Image: host("Image"), Pressable: host("Pressable"), ScrollView: host("ScrollView"), Text: host("Text"), View: host("View"),
		Platform: { OS: "ios" }, StyleSheet: { create: (styles: object) => styles, hairlineWidth: 1 },
		useWindowDimensions: () => ({ width: 390, height: 844, fontScale: 0.85 }),
	};
});
vi.mock("react-native-gesture-handler", async () => {
	const { createElement } = await import("react");
	return {
		FlatList: (props: FlatListFixture) => {
			const cited = props.data[3999];
			return createElement("FlatList", props, cited ? props.renderItem({ item: cited, index: 3999 }) : null);
		},
		GestureDetector: ({ children }: { children: React.ReactNode }) => children,
	};
});
vi.mock("react-native-keyboard-controller", () => ({}));
vi.mock("react-native-safe-area-context", () => ({ useSafeAreaInsets: () => ({ bottom: 0 }) }));
vi.mock("react-native-svg", () => ({ SvgXml: "SvgXml" }));
vi.mock("expo-clipboard", () => ({}));
vi.mock("expo-router", () => {
	const navigation = { setOptions: vi.fn() };
	const toolbar = Object.assign(() => null, { Menu: () => null, MenuAction: () => null });
	return {
		Stack: { Toolbar: toolbar },
		useLocalSearchParams: () => ({ sessionId: "session", path: "long.txt", mode: "file", line: "4000", hostId: "local" }),
		useNavigation: () => navigation,
		useRouter: () => ({}),
	};
});
vi.mock("../ThemeProvider", async () => {
	const { darkTheme } = await import("../theme");
	return { useTheme: () => darkTheme, useThemedStyles: (factory: (theme: typeof darkTheme) => object) => factory(darkTheme) };
});
vi.mock("../store", () => ({
	HostScope: ({ children }: { children: React.ReactNode }) => children,
	useApp: () => ({ currentHostId: "local" }),
}));
vi.mock("./useFilesBackend", () => {
	const backend = {
		file: async () => ({ status: "unmodified", content: "example\n".repeat(4001) }),
	};
	return { useFilesBackend: () => ({ backend }) };
});
vi.mock("./useChangeList", () => ({
	useBranchBackend: (backend: unknown) => backend,
	useChangeList: () => ({}),
	useRouteSelection: () => ({ branch: { kind: "checked_out" }, changes: { kind: "all" } }),
}));
vi.mock("./useWorkspace", () => ({ useWorkspaceManifest: () => ({ manifest: null }) }));
vi.mock("./useLineDrag", () => {
	const reset = vi.fn();
	return { useLineDrag: () => ({ reset }) };
});
vi.mock("./useReviewSender", () => ({ useReviewSender: () => ({ all: [], comments: [] }) }));
vi.mock("./reviewStore", () => ({ reviewKey: () => "session" }));
vi.mock("./useFileActions", () => ({ useFileActions: () => ({}) }));
vi.mock("./fileIcons", () => ({ fileIcon: () => "fixture-icon" }));
vi.mock("./ReviewButton", () => ({ ReviewButton: () => null }));
vi.mock("./SegmentedChoice", () => ({ SegmentedChoice: () => null }));
vi.mock("../chat/ChatMarkdown", () => ({ ChatMarkdown: () => null }));
vi.mock("../haptics", () => ({ haptics: {} }));
vi.mock("../ImageViewer", () => ({ ImageViewer: () => null }));
vi.mock("../RouteErrorBoundary", () => ({ RouteErrorBoundary: () => null }));
vi.mock("../Spinner", () => ({ Spinner: () => null }));
vi.mock("../Toast", () => ({ ToastHost: () => null }));
vi.mock("../ui", () => ({ EmptyState: () => null }));
vi.mock("../useHeaderOverlap", () => ({ useGlassBarInset: () => 0 }));

import SessionFileScreen from "../../app/files/[sessionId]/file";

type FlatListFixture = {
	data: DisplayRow[];
	initialScrollIndex: number;
	getItemLayout(data: DisplayRow[], index: number): { length: number; offset: number; index: number };
	renderItem(row: { item: DisplayRow; index: number }): React.ReactNode;
};

it("opens a cited line at the native row offset with smaller system text", async () => {
	vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
	let rendered: ReactTestRenderer | undefined;
	try {
		await act(async () => { rendered = create(<SessionFileScreen />); });
		const list = rendered!.root.findByType("FlatList" as never).props as FlatListFixture;
		expect(list.initialScrollIndex).toBe(3995);
		const layout = list.getItemLayout(list.data, list.initialScrollIndex);
		const line = rendered!.root.findByType("Pressable" as never);
		expect(line.props.accessibilityLabel).toBe("Line 4000, linked line: example");
		const gutter = line.findByType("View" as never);
		const rowStyle = Object.assign({}, ...line.props.style);
		const gutterStyle = Object.assign({}, ...gutter.props.style);
		const nativeHeight = Math.max(rowStyle.height, rowStyle.minHeight ?? 0, gutterStyle.minHeight ?? 0);
		expect(nativeHeight).toBe(17);
		expect(layout).toEqual({ length: nativeHeight, offset: 3995 * nativeHeight, index: 3995 });
	} finally {
		if (rendered) await act(async () => { rendered?.unmount(); });
		vi.unstubAllGlobals();
	}
});
