import "../../src/renderer/styles.css";
import { applyDocumentTheme, applyDocumentThemeStyle } from "../../src/renderer/lib/theme";
import { DEFAULT_THEME_STYLE } from "../../src/renderer/lib/theme-palettes";

const theme = new URLSearchParams(location.search).get("theme");
applyDocumentThemeStyle(theme === "dark" ? "dracula" : DEFAULT_THEME_STYLE);
applyDocumentTheme(theme === "dark" ? "dark" : "light");
