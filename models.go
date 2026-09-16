package main

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// registrationResponse advertises the plugin metadata and the capabilities the
// plugin actually implements. Only implemented capabilities are declared:
// CLIProxyAPI rejects or mis-routes a plugin that over-declares.
func registrationResponse() map[string]any {
	return map[string]any{
		"schema_version": pluginabi.SchemaVersion,
		"metadata": pluginapi.Metadata{
			Name:             "CodeArts",
			Version:          pluginVersion,
			Author:           "converted from huaweicloud.vscode-codebot",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "base_url",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "CodeArts Doer regional API base URL, for example https://snap-access.cn-north-4.myhuaweicloud.com.",
				},
				{
					Name:        "api_mode",
					Type:        pluginapi.ConfigFieldTypeEnum,
					EnumValues:  []string{"agent", "native"},
					Description: "Upstream protocol: agent uses the OpenAI-compatible /api/v2/chat/completions endpoint, native uses /v1/chat/chat with CodeArts SSE framing.",
				},
				{
					Name:        "web_login_base",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "CodeArts web console base URL used to start the browser login flow.",
				},
				{
					Name:        "plugin_name",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Value sent as the plugin-name header. Defaults to snap_vscode.",
				},
				{
					Name:        "plugin_version",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Value sent as the plugin-version and client_version headers.",
				},
				{
					Name:        "language",
					Type:        pluginapi.ConfigFieldTypeEnum,
					EnumValues:  []string{"zh-cn", "en-us"},
					Description: "Value sent as the X-Language header.",
				},
				{
					Name:        "agent_id",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "CodeArts agent UUID or alias used by the native protocol.",
				},
				{
					Name:        "default_model_id",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Upstream model_id sent when the requested model has no explicit mapping.",
				},
				{
					Name:        "models",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Static model list advertised to CLIProxyAPI.",
				},
				{
					Name:        "model_map",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Map of client-facing model IDs to upstream model IDs.",
				},
				{
					Name:        "schedule",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Cron-driven plugin tasks. The plugin ABI has no timer, so the plugin schedules its own work (token_renew, quota_refresh, or an arbitrary http request).",
				},
				{
					Name:        "sign_host",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Include the host header in the signed header set. Off by default because the official extension does not sign host.",
				},
				{
					Name:        "is_confidential",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Set the is_confidential header for KIA (confidential code) mode.",
				},
				{
					Name:        "heartbeat",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Request upstream SSE heartbeat comment frames.",
				},
				{
					Name:        "request_timeout_seconds",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Timeout in seconds for a single upstream request.",
				},
				{
					Name:        "insist_missing_credentials",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Send unsigned upstream requests when no credential is available. Debugging only.",
				},
				{
					Name:        "extra_headers",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Additional headers added to every upstream request and included in the signature.",
				},
			},
		},
		"capabilities": declaredCapabilities(),
	}
}

// modelRegistration returns the models contributed to the registry.
func modelRegistration() pluginapi.ModelRegistrationResponse {
	return pluginapi.ModelRegistrationResponse{
		Provider: providerID,
		Models:   modelInfos(),
	}
}

// staticModels returns the provider-native static model list. The upstream model
// catalogue is tenant specific and normally fetched at runtime by the official
// IDE plugin, so the plugin advertises the configured list instead.
func staticModels() pluginapi.ModelResponse {
	return pluginapi.ModelResponse{
		Provider: providerID,
		Models:   modelInfos(),
	}
}

func modelInfos() []pluginapi.ModelInfo {
	cfg := config()
	now := time.Now().Unix()
	out := make([]pluginapi.ModelInfo, 0, len(cfg.Models))
	for _, model := range cfg.Models {
		if model.ID == "" {
			continue
		}
		out = append(out, pluginapi.ModelInfo{
			ID:                         model.ID,
			Object:                     "model",
			Created:                    now,
			OwnedBy:                    providerID,
			Type:                       "chat",
			DisplayName:                model.DisplayName,
			Name:                       model.Name,
			Description:                model.Description,
			InputTokenLimit:            model.ContextLength,
			OutputTokenLimit:           model.MaxOutputTokens,
			ContextLength:              model.ContextLength,
			MaxCompletionTokens:        model.MaxOutputTokens,
			SupportedGenerationMethods: []string{"chat.completions"},
			SupportedInputModalities:   []string{"text"},
			SupportedOutputModalities:  []string{"text"},
			SupportedParameters: []string{
				"temperature",
				"top_p",
				"max_tokens",
				"stream",
				"reasoning_effort",
			},
		})
	}
	return out
}

// declaredCapabilities is the single source of truth for the capability set.
// Both plugin.register and the management status page render it, so the plugin
// cannot advertise one set while registering another.
//
// Intentionally unsupported capabilities are listed as false rather than merely
// omitted, so the decision is visible and greppable. See
// docs/capability-matrix.md for why each is not implemented.
func declaredCapabilities() map[string]any {
	return map[string]any{
		// Implemented.
		"model_provider":          true,
		"auth_provider":           true,
		"executor":                true,
		"executor_model_scope":    pluginapi.ExecutorModelScopeBoth,
		"executor_input_formats":  []string{"chat-completions"},
		"executor_output_formats": []string{"chat-completions"},
		"management_api":          true,
		"quota_provider":          true,
		"usage_plugin":            true,
		"thinking_applier":        true,
		"scheduler":               true,

		// Not implemented.
		"model_registrar":             false,
		"frontend_auth_provider":      false,
		"model_router":                false,
		"request_translator":          false,
		"request_normalizer":          false,
		"request_interceptor":         false,
		"request_lifecycle_plugin":    false,
		"response_translator":         false,
		"response_before_translator":  false,
		"response_after_translator":   false,
		"response_interceptor":        false,
		"stream_chunk_interceptor":    false,
		"websocket_response_observer": false,
		"command_line_plugin":         false,
	}
}
