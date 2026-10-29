import { afterEach, expect, it } from "vitest";
import { appI18n } from "../i18n";
import { appCatalogs } from "../i18n/all-messages";
import { APP_LOCALES } from "../i18n/locales";
import { agentAccountErrorMessage } from "./useAgentAccounts";

afterEach(async () => { await appI18n.changeLanguage("en"); });

it.each(APP_LOCALES)("localizes account diagnostics in %s without machine-code fallbacks", async (locale) => {
	await appI18n.changeLanguage(locale);
	for (const [code, key] of [
		["account_unavailable_home_occupied", "settings.agentAccounts.unavailableHomeOccupied"],
		["AGENT_ACCOUNT_UNAVAILABLE_HOME_OCCUPIED", "settings.agentAccounts.unavailableHomeOccupied"],
		["AGENT_ACCOUNT_IN_USE", "settings.agentAccounts.cleanupScheduled"],
		["account_identity_mismatch", "settings.agentAccounts.identityMismatch"],
		["account_provider_unavailable", "settings.agentAccounts.providerUnavailable"],
		["account_credential_store_unsupported", "settings.agentAccounts.credentialStoreUnsupported"],
		["AGENT_ACCOUNT_CREDENTIAL_STORE_UNSUPPORTED", "settings.agentAccounts.credentialStoreUnsupported"],
	]) {
		const expected = appCatalogs[locale][key];
		expect(expected).toBeTruthy();
		expect(agentAccountErrorMessage(code)).toBe(expected);
		expect(agentAccountErrorMessage(code)).not.toContain(code);
	}
});
