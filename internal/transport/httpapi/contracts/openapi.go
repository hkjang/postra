package contracts

import "github.com/google/jsonschema-go/jsonschema"

type endpoint struct {
	method, path, id, summary, request, response, status string
	list                                                 bool
}

func endpoints() []endpoint {
	return []endpoint{
		{"get", "/admin/configuration", "adminConfigurationGet", "Administrator catalog with effective values and lock/source metadata", "", "SettingsView", "200", false},
		{"patch", "/admin/configuration", "adminConfigurationPatch", "Administrator settings and write-only secret update", "SettingsPatch", "SettingsView", "200", false},
		{"get", "/preferences", "preferencesGet", "Current user's effective preferences", "", "SettingsView", "200", false},
		{"patch", "/preferences", "preferencesPatch", "Update current user's unlocked preferences", "SettingsPatch", "SettingsView", "200", false},
		{"get", "/accounts/{id}/preferences", "accountPreferencesGet", "Effective preferences for an owned account", "", "SettingsView", "200", false},
		{"patch", "/accounts/{id}/preferences", "accountPreferencesPatch", "Update an owned account's unlocked preferences", "SettingsPatch", "SettingsView", "200", false},
		{"post", "/qa", "mailAsk", "Answer a question using bounded owned-mail/work/action evidence", "AskInput", "AskResult", "200", false},
		{"post", "/mail/render", "mailRender", "Render locally; optional explicit smart_format uses AI but never sends", "RenderMailInput", "RenderedMail", "200", false},
		{"get", "/mail/templates", "mailTemplates", "List deterministic built-in templates", "", "MailTemplate", "200", true},
		{"post", "/drafts", "draftCreate", "Create a draft without approving or sending", "CreateDraftInput", "DraftView", "201", false},
		{"get", "/drafts/{id}", "draftGet", "Read the current version of an owned draft", "", "DraftView", "200", false},
		{"patch", "/drafts/{id}", "draftUpdate", "Create a new immutable version; invalidates earlier approval", "UpdateDraftInput", "DraftView", "200", false},
		{"post", "/drafts/{id}/preview", "draftSendPreview", "Review canonical send payload and policies; does not issue approval", "", "SendPreview", "200", false},
		{"post", "/drafts/{id}/attachments", "draftAttachmentAdd", "Add a scanned attachment in a new draft version", "AddDraftAttachmentInput", "DraftView", "201", false},
		{"delete", "/drafts/{id}/attachments/{attachment}", "draftAttachmentRemove", "Remove an attachment from a new version, retaining history", "", "DraftView", "200", false},
		{"get", "/signatures", "signaturesList", "List only the current user's signatures", "", "MailSignature", "200", true},
		{"post", "/signatures", "signatureCreate", "Create an owned signature", "SaveMailSignatureInput", "MailSignature", "201", false},
		{"get", "/signatures/{id}", "signatureGet", "Read an owned signature; administrator role does not bypass ownership", "", "MailSignature", "200", false},
		{"put", "/signatures/{id}", "signatureUpdate", "Update an owned signature; path ID is authoritative", "SaveMailSignatureInput", "MailSignature", "200", false},
	}
}
func schemaRef(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}
func jsonContent(schema any) map[string]any {
	return map[string]any{"application/json": map[string]any{"schema": schema}}
}

func openAPIDocument(schemas map[string]*jsonschema.Schema) map[string]any {
	paths := map[string]map[string]any{}
	for _, entry := range endpoints() {
		var response any = schemaRef(entry.response)
		if entry.list {
			response = map[string]any{"type": "array", "items": response}
		}
		operation := map[string]any{"operationId": entry.id, "summary": entry.summary, "responses": map[string]any{
			entry.status: map[string]any{"description": "Success", "content": jsonContent(response)},
			"default":    map[string]any{"description": "Classified error; dynamic ownership, scopes, policy and revision checks remain authoritative. Legacy error alias may also be present.", "content": jsonContent(schemaRef("ErrorResponse"))},
		}}
		parameters := []any{}
		for _, name := range []string{"id", "attachment"} {
			if containsParameter(entry.path, name) {
				parameters = append(parameters, map[string]any{"name": name, "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
			}
		}
		if entry.method != "get" {
			parameters = append(parameters, map[string]any{"name": "X-CSRF-Token", "in": "header", "required": false, "description": "Required with cookie authentication, together with an allowed same-origin Origin header. Bearer requests follow their separate scope policy.", "schema": map[string]any{"type": "string"}})
		}
		if len(parameters) > 0 {
			operation["parameters"] = parameters
		}
		if entry.request != "" {
			content := jsonContent(schemaRef(entry.request))
			if entry.id == "draftAttachmentAdd" {
				content["multipart/form-data"] = map[string]any{"schema": map[string]any{"type": "object", "required": []string{"file"}, "properties": map[string]any{"file": map[string]any{"type": "string", "format": "binary"}}, "additionalProperties": false}}
				operation["parameters"] = append(parameters, map[string]any{"name": "inline", "in": "query", "description": "Multipart upload only: true inserts a validated PNG/JPEG/GIF as a CID image. JSON uploads use their inline field.", "schema": map[string]any{"type": "boolean", "default": false}})
			}
			operation["requestBody"] = map[string]any{"required": true, "content": content}
		}
		if paths[entry.path] == nil {
			paths[entry.path] = map[string]any{}
		}
		paths[entry.path][entry.method] = operation
	}
	return map[string]any{
		"openapi": "3.1.0", "jsonSchemaDialect": schemaDialect,
		"info":               map[string]any{"title": "Postra canonical core API — partial coverage", "version": "1", "description": "Generated from Go DTOs for settings, Ask, rendering, drafts, preview, attachment mutation and signatures only. This is not the complete Postra REST/MCP API. /api/v1 is canonical; /api is a compatibility alias using the same handlers. Sending/approval, binary downloads, auth, admin probes, and other operations are intentionally outside this snapshot. JSON schemas describe wire shapes, not every dynamic validation or permission rule."},
		"servers":            []any{map[string]any{"url": "/api/v1", "description": "Canonical API v1"}},
		"x-legacy-base-path": "/api", "x-generated-by": "go run ./cmd/postra-contracts",
		"security": []any{map[string]any{"sessionCookie": []string{}}, map[string]any{"bearerAuth": []string{}}},
		"components": map[string]any{"schemas": schemas, "securitySchemes": map[string]any{
			"sessionCookie": map[string]any{"type": "apiKey", "in": "cookie", "name": "postra_session", "description": "Browser session; mutations additionally require same-origin CSRF checks."},
			"bearerAuth":    map[string]any{"type": "http", "scheme": "bearer", "description": "Scoped Postra MCP key or supported deployment bearer credentials. Capabilities and ownership are enforced by the server."},
		}}, "paths": paths,
	}
}
func containsParameter(path, name string) bool {
	for i := 0; i+len(name)+2 <= len(path); i++ {
		if path[i:i+len(name)+2] == "{"+name+"}" {
			return true
		}
	}
	return false
}
