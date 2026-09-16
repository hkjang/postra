package mcpserver

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"

	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/mailrender"
	"postra/internal/transport/httpapi/contracts"
)

// This registry describes the actual tool wire envelopes, not the REST route
// envelopes. DTO inference shares encoding/json nullable/embedded-field rules
// with REST. Every canonical registered tool has a typed success contract;
// aliases inherit the same schema. Errors use the gateway ErrorResponse with
// isError=true and are deliberately outside the successful output schema.
func outputTypes() map[string]reflect.Type {
	out := map[string]reflect.Type{}
	add := func(value any, names ...string) {
		for _, name := range names {
			out[name] = reflect.TypeOf(value)
		}
	}
	add(capabilitiesOutput{}, "mail_capabilities")
	add(domain.Principal{}, "mail_identity")
	add(systemInfoOutput{}, "mail_system_info")
	add(domain.MailAccount{}, "mail_account_get", "mail_account_create", "mail_account_update")
	add(struct {
		Accounts []domain.MailAccount `json:"accounts"`
	}{}, "mail_account_list")
	add(struct {
		Diagnostics []domain.ConnDiagnostics `json:"diagnostics"`
	}{}, "mail_account_test")
	add(struct {
		Instructions string `json:"instructions"`
	}{}, "secret_registration_begin", "secret_rotation_begin")
	add(struct {
		Status string `json:"status"`
	}{}, "mail_account_disable", "secret_revoke", "job_cancel", "mail_rule_delete", "mail_draft_delete", "mail_signature_delete")
	add(struct {
		Status    string `json:"status"`
		MessageID string `json:"message_id"`
	}{}, "mail_local_delete")
	add(domain.Job{}, "mail_sync_start", "job_status", "mail_embeddings_build")
	add(struct {
		Jobs []domain.Job `json:"jobs"`
	}{}, "job_list")
	add(domain.SearchResult{}, "mail_search")
	add(application.MessageView{}, "mail_message_get")
	add(application.ThreadView{}, "mail_thread_get")
	add(struct {
		Attachments []domain.Attachment `json:"attachments"`
	}{}, "mail_attachment_list")
	add(struct {
		Results []application.MessageView `json:"results"`
		Count   int                       `json:"count"`
	}{}, "mail_hybrid_search")
	add(struct {
		Results []application.MessageView `json:"results"`
	}{}, "mail_semantic_search")
	add(struct {
		Timeline []application.MessageView `json:"timeline"`
		Count    int                       `json:"count"`
	}{}, "mail_thread_timeline")
	add(application.WorkInbox{}, "mail_work_inbox")
	add(application.BatchResult{}, "mail_batch_update")
	add(domain.Analysis{}, "mail_summarize", "mail_classify", "mail_action_items_extract", "mail_entities_extract", "mail_phishing_inspect", "mail_thread_summarize", "mail_attachment_summarize", "mail_daily_digest")
	add(application.AttachmentText{}, "mail_attachment_extract_text")
	add(domain.EmailAuthResult{}, "mail_auth_inspect")
	add(application.EvalResult{}, "mail_eval_prompt")
	add(application.AskResult{}, "mail_question_answer")
	add(application.CalendarEvents{}, "mail_calendar_extract")
	add(application.ReplySuggestions{}, "mail_suggest_replies")
	add(struct {
		Rule  domain.MailRule `json:"rule"`
		Saved bool            `json:"saved"`
	}{}, "mail_rule_draft_from_text")
	add(application.DraftView{}, "mail_draft_get", "mail_draft_create", "mail_draft_update", "mail_draft_rewrite", "mail_draft_attachment_add", "mail_draft_attachment_remove")
	add(application.DraftList{}, "mail_drafts_list")
	add(attachmentDownloadOutput{}, "mail_draft_attachment_get")
	add(application.SendPreview{}, "mail_send_preview")
	add(sendApprovalOutput{}, "mail_send_request_approval")
	add(domain.OutboundMessage{}, "mail_send", "mail_outbound_status")
	add(struct {
		Outbound []application.OutboundView `json:"outbound"`
	}{}, "mail_outbound_list")
	add(mailrender.Output{}, "mail_render")
	add(struct {
		Text string `json:"text"`
	}{}, "mail_text_rewrite")
	add(struct {
		Templates []mailrender.Template `json:"templates"`
	}{}, "mail_templates")
	add(struct {
		Signatures []domain.MailSignature `json:"signatures"`
	}{}, "mail_signature_list")
	add(domain.MailSignature{}, "mail_signature_get", "mail_signature_save")
	add(application.ServerDeletePreview{}, "mail_server_delete_preview")
	add(struct {
		Preview  application.ServerDeletePreview `json:"preview"`
		Approval domain.ApprovalToken            `json:"approval"`
	}{}, "mail_server_delete_request_approval")
	add(application.ServerDeleteResult{}, "mail_server_delete")
	add(struct {
		Rules []domain.MailRule `json:"rules"`
	}{}, "mail_rules_list")
	add(domain.MailRule{}, "mail_rule_create", "mail_rule_update")
	add(application.RuleRunResult{}, "mail_apply_rules")
	add(struct {
		Cards []domain.ActionCard `json:"cards"`
		Count int                 `json:"count"`
	}{}, "mail_action_cards_extract")
	add(struct {
		Cards []domain.ActionCard `json:"cards"`
	}{}, "mail_action_cards_list")
	add(domain.ActionCard{}, "mail_action_card_create", "mail_action_card_set_status")
	add(application.ActionCardExport{}, "mail_action_card_export")
	add(struct {
		Items []application.TeamInboxItem `json:"items"`
		Count int                         `json:"count"`
	}{}, "mail_team_inbox")
	add(application.MessageCollabView{}, "mail_collab_get")
	add(domain.MessageCollab{}, "mail_assign", "mail_set_work_status", "mail_set_sla")
	add(domain.MessageNote{}, "mail_add_note")
	add(struct {
		Events []domain.AuditEvent `json:"events"`
	}{}, "mail_audit_search")
	add(application.NotificationSnapshot{}, "mail_events")
	return out
}

type sendApprovalOutput struct {
	Preview  application.SendPreview `json:"preview"`
	Approval domain.ApprovalToken    `json:"approval"`
}

type attachmentDownloadOutput struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	MIMEType    string `json:"mime_type"`
	Size        int64  `json:"size"`
	Inline      bool   `json:"inline"`
	ContentID   string `json:"content_id"`
	ScanStatus  string `json:"scan_status"`
	DownloadURL string `json:"download_url"`
	DataBase64  string `json:"data_base64,omitempty"`
}

type systemInfoOutput struct {
	Name           string   `json:"name"`
	Version        string   `json:"version"`
	Transports     []string `json:"transports"`
	OfflineCapable bool     `json:"offline_capable"`
	MailPrivacy    string   `json:"mail_privacy"`
}

type capabilitiesOutput struct {
	ProtocolVersion string              `json:"protocol_version"`
	Groups          map[string][]string `json:"groups"`
	Access          map[string]string   `json:"access"`
	AccessModel     struct {
		Levels      []string `json:"levels"`
		Admin       string   `json:"admin"`
		User        string   `json:"user"`
		Other       string   `json:"other"`
		Overridable string   `json:"overridable"`
	} `json:"access_model"`
	Note               string                   `json:"note"`
	Scopes             []string                 `json:"scopes"`
	Aliases            map[string]string        `json:"aliases"`
	DefaultKeyScopes   []string                 `json:"default_key_scopes"`
	ApprovalFlow       []string                 `json:"approval_flow"`
	ErrorContract      []string                 `json:"error_contract"`
	Enabled            bool                     `json:"enabled"`
	HTTPEnabled        bool                     `json:"http_enabled"`
	Endpoint           string                   `json:"endpoint"`
	ConfiguredEndpoint string                   `json:"configured_endpoint"`
	PendingRestart     bool                     `json:"pending_restart"`
	OAuth              application.MCPOAuthInfo `json:"oauth"`
	Permissions        map[string]bool          `json:"permissions"`
	RequestTimeoutSec  int                      `json:"request_timeout_sec"`
	GrantedScopes      []string                 `json:"granted_scopes,omitempty"`
	Principal          *domain.Principal        `json:"principal,omitempty"`
}

var inferredOutputSchemas = sync.OnceValues(func() (map[string]*jsonschema.Schema, error) {
	out := map[string]*jsonschema.Schema{}
	for name, typ := range outputTypes() {
		schema, err := contracts.SchemaForType(typ)
		if err != nil {
			return nil, fmt.Errorf("MCP output %s: %w", name, err)
		}
		schema.Title = name + " success"
		out[name] = schema
	}
	return out, nil
})

func outputSchema(name string) *jsonschema.Schema {
	schemas, err := inferredOutputSchemas()
	if err != nil {
		panic(err)
	}
	schema := schemas[application.CanonicalMCPTool(name)]
	if schema == nil {
		panic("MCP success contract is missing: " + name)
	}
	return schema
}
