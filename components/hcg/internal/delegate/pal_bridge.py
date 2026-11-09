"""One-shot PAL chat adapter. Only the public provider API crosses stdio."""
import asyncio
import json
import sys

if sys.version_info[:3] != (3, 12, 3):
    raise RuntimeError("fixed Python runtime unavailable")

sys.path.insert(0, "/pal")
from providers.custom import CustomProvider
from providers.registry import ModelProviderRegistry
from providers.shared import ModelCapabilities, ModelResponse, ProviderType
from tools.chat import ChatTool

request = json.loads(sys.stdin.readline())
profile = request["profile"]


class BrokerCustom(CustomProvider):
    def __init__(self, api_key=""):
        endpoint = {"openai": "https://api.openai.com/v1/", "anthropic": "https://api.anthropic.com/v1/"}[profile["billingProvider"]]
        super().__init__(api_key="", base_url=endpoint)
        self.sent = False
        self.capabilities = ModelCapabilities(
            provider=ProviderType.CUSTOM,
            model_name=profile["model"],
            friendly_name="HCG fixed delegation",
            context_window=profile["contextTokens"],
            max_output_tokens=profile["outputTokens"],
            supports_temperature=False,
            supports_streaming=False,
            allow_code_generation=False,
        )

    def get_capabilities(self, model_name):
        if model_name != profile["model"]:
            raise ValueError("unregistered model")
        return self.capabilities

    def get_all_model_capabilities(self):
        return {profile["model"]: self.capabilities}

    def validate_model_name(self, model_name):
        return model_name == profile["model"]

    def generate_content(self, prompt, model_name, system_prompt=None, temperature=None,
                         max_output_tokens=None, images=None, **kwargs):
        if self.sent or images or model_name != profile["model"]:
            raise ValueError("attempt or model not authorized")
        self.sent = True
        print(json.dumps({"kind": "provider", "prompt": prompt, "system": system_prompt,
                          "model": model_name}), flush=True)
        reply = json.loads(sys.stdin.readline())
        if reply.get("error"):
            raise ValueError("broker unavailable")
        return ModelResponse(content=reply["text"], usage=reply["usage"],
                             model_name=model_name, provider=ProviderType.CUSTOM)


ModelProviderRegistry.register_provider(ProviderType.CUSTOM, lambda api_key: BrokerCustom())
# Public discovery and execution use the same sole registered provider.
tool = ChatTool()
tool.get_input_schema()
import server
if set(server.TOOLS) != {"chat", "version", "listmodels"}:
    raise ValueError("unexpected tool discovery")
response = asyncio.run(tool.execute({"prompt": request["question"], "model": profile["model"],
    "absolute_file_paths": request["paths"], "working_directory_absolute_path": "/view"}))
ok = bool(response) and all(json.loads(item.text).get("status") != "error" for item in response)
print(json.dumps({"kind": "done", "ok": ok}), flush=True)
