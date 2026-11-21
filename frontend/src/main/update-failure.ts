import type { MessageBoxOptions, MessageBoxReturnValue } from "electron";
import type { AppLocale } from "../shared/ui-locale";

const updateFailureCopy = {
	en: ["Update failed. Restart AO.", "Restart", "Close"],
	"zh-TW": ["更新失敗，請重新啟動 AO", "重新啟動", "關閉"],
	"zh-CN": ["更新失败，请重新启动 AO", "重新启动", "关闭"],
	ja: ["アップデートに失敗しました。AO を再起動してください。", "再起動", "閉じる"],
	ko: ["업데이트에 실패했습니다. AO를 다시 시작하세요.", "다시 시작", "닫기"],
	de: ["Update fehlgeschlagen. Starte AO neu.", "Neu starten", "Schließen"],
	es: ["La actualización falló. Reinicia AO.", "Reiniciar", "Cerrar"],
	fr: ["La mise à jour a échoué. Redémarrez AO.", "Redémarrer", "Fermer"],
	"pt-BR": ["A atualização falhou. Reinicie o AO.", "Reiniciar", "Fechar"],
	ru: ["Не удалось обновить AO. Перезапустите приложение.", "Перезапустить", "Закрыть"],
	tr: ["Güncelleme başarısız oldu. AO’yu yeniden başlatın.", "Yeniden başlat", "Kapat"],
} satisfies Record<AppLocale, readonly [message: string, restart: string, close: string]>;

export async function showUpdateFailure(locale: AppLocale, options: {
	showMessageBox: (options: MessageBoxOptions) => Promise<Pick<MessageBoxReturnValue, "response">>;
	relaunch(): void;
}): Promise<void> {
	const [message, restart, close] = updateFailureCopy[locale];
	const result = await options.showMessageBox({
		type: "error",
		message,
		buttons: [restart, close],
		defaultId: 1,
		cancelId: 1,
		noLink: true,
	});
	if (result.response === 0) options.relaunch();
}
