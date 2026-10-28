import { useTranslation } from "react-i18next";
import { useSettings, useUpdateReplyLanguage, type ReplyLanguage } from "../../hooks/useSettings";
import { SettingsOptionMenu, type SettingsOption } from "./SettingsOptionMenu";
import { SettingsRow } from "./SettingsRow";

export function ReplyLanguageRow() {
	const { t } = useTranslation();
	const { settings, isLoading, error: loadError } = useSettings();
	const { update, saving, error: saveError } = useUpdateReplyLanguage();
	const error = saveError ?? loadError;
	const options = [
		{ value: "", label: t("settings.replyLanguage.off") },
		{ value: "zh-TW", label: "繁體中文" },
		{ value: "en", label: "English" },
		{ value: "zh-CN", label: "简体中文" },
		{ value: "ja", label: "日本語" },
		{ value: "ko", label: "한국어" },
		{ value: "es", label: "Español" },
		{ value: "fr", label: "Français" },
		{ value: "de", label: "Deutsch" },
		{ value: "pt-BR", label: "Português (Brasil)" },
	] satisfies SettingsOption<ReplyLanguage>[];
	return (
		<SettingsRow label={t("settings.replyLanguage.label")} description={<>
			{t("settings.replyLanguage.description")}
			{error ? <span role="alert" className="block text-error">{error}</span> : null}
		</>}>
			<SettingsOptionMenu
				aria-label={t("settings.replyLanguage.label")}
				value={settings?.replyLanguage ?? ""}
				options={options}
				onChange={update}
				disabled={isLoading || saving || Boolean(loadError) || !settings}
			/>
		</SettingsRow>
	);
}
