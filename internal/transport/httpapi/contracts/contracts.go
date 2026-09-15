// Package contracts generates the intentionally bounded canonical REST contract
// from the same Go DTOs used by the handlers. It is a build/test tool, not runtime
// request validation or a replacement for application ownership/policy checks.
package contracts

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/mailrender"
)

const SchemaPath = "api/contracts.schema.json"
const OpenAPIPath = "api/openapi.json"
const TypeScriptPath = "web/src/api/contracts.generated.ts"
const schemaDialect = "https://json-schema.org/draft/2020-12/schema"

// Registry is an explicit coverage boundary, not an inventory of every Postra
// route. Adding/changing a Go field updates JSON Schema, OpenAPI and TypeScript.
func Registry() map[string]reflect.Type {
	return map[string]reflect.Type{
		"SettingDefinition":       reflect.TypeFor[application.SettingDefinition](),
		"EffectiveSetting":        reflect.TypeFor[application.EffectiveSetting](),
		"SettingsView":            reflect.TypeFor[application.SettingsView](),
		"SettingsPatch":           reflect.TypeFor[application.SettingsPatch](),
		"AskInput":                reflect.TypeFor[application.AskInput](),
		"AskSource":               reflect.TypeFor[application.AskSource](),
		"AskRetrieval":            reflect.TypeFor[application.AskRetrieval](),
		"AskResult":               reflect.TypeFor[application.AskResult](),
		"RenderMailInput":         reflect.TypeFor[application.RenderMailInput](),
		"RenderedMail":            reflect.TypeFor[mailrender.Output](),
		"MailTemplate":            reflect.TypeFor[mailrender.Template](),
		"CreateDraftInput":        reflect.TypeFor[application.CreateDraftInput](),
		"UpdateDraftInput":        reflect.TypeFor[application.UpdateDraftInput](),
		"DraftView":               reflect.TypeFor[application.DraftView](),
		"SendPreview":             reflect.TypeFor[application.SendPreview](),
		"DraftAttachment":         reflect.TypeFor[domain.DraftAttachment](),
		"AddDraftAttachmentInput": reflect.TypeFor[application.AddDraftAttachmentInput](),
		"MailSignature":           reflect.TypeFor[domain.MailSignature](),
		"SaveMailSignatureInput":  reflect.TypeFor[application.SaveMailSignatureInput](),
		"ErrorResponse":           reflect.TypeFor[domain.ErrorResponse](),
	}
}

func Schemas() (map[string]*jsonschema.Schema, error) {
	out := map[string]*jsonschema.Schema{}
	for name, typ := range Registry() {
		schema, err := SchemaForType(typ)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		schema.Title = name
		out[name] = schema
	}
	// These documented transport defaults differ from Go's required-on-marshal
	// tags: the URL supplies draft_id; reset-only PATCH needs no values object;
	// an omitted draft kind means new. Unknown request keys are decoder-compatible.
	for _, name := range []string{"SettingsPatch", "AskInput", "RenderMailInput", "CreateDraftInput", "UpdateDraftInput", "AddDraftAttachmentInput", "SaveMailSignatureInput"} {
		out[name].AdditionalProperties = nil
	}
	out["SettingsPatch"].Required = nil
	out["SettingsPatch"].Properties["secrets"].WriteOnly = true
	out["SettingsPatch"].Properties["secrets"].Description = "Write-only replacement secrets. Never returned by settings reads."
	optional(out["CreateDraftInput"], "kind")
	optional(out["UpdateDraftInput"], "draft_id")
	optional(out["AddDraftAttachmentInput"], "draft_id")
	out["CreateDraftInput"].Properties["kind"].Default = json.RawMessage(`"new"`)
	out["ErrorResponse"].AdditionalProperties = nil // legacy `error` alias may accompany the canonical fields
	return out, nil
}

// SchemaForType infers a standalone Go wire schema, including nullable maps
// and nil embedded pointer fields. MCP output wrappers can reuse it without
// importing HTTP handlers or applying REST-only request defaults.
func SchemaForType(typ reflect.Type) (*jsonschema.Schema, error) {
	schema, err := jsonschema.ForType(typ, nil)
	if err != nil {
		return nil, err
	}
	adjustWireSchema(typ, schema)
	return schema, nil
}

// Align the inferencer with encoding/json: nil maps are null, and fields
// promoted from a nil embedded pointer are absent (not required). Slices and
// ordinary pointers are already nullable in jsonschema-go's wire inference.
func adjustWireSchema(typ reflect.Type, schema *jsonschema.Schema) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Map:
		makeNullable(schema)
		if schema.AdditionalProperties != nil {
			adjustWireSchema(typ.Elem(), schema.AdditionalProperties)
		}
	case reflect.Slice, reflect.Array:
		if schema.Items != nil {
			adjustWireSchema(typ.Elem(), schema.Items)
		}
	case reflect.Struct:
		for _, field := range reflect.VisibleFields(typ) {
			if field.Anonymous || field.PkgPath != "" {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			child := schema.Properties[name]
			if child == nil {
				continue
			}
			adjustWireSchema(field.Type, child)
			parent := typ
			for _, index := range field.Index[:len(field.Index)-1] {
				parent = parent.Field(index).Type
				if parent.Kind() == reflect.Pointer {
					optional(schema, name)
					parent = parent.Elem()
				}
			}
		}
	}
}
func makeNullable(schema *jsonschema.Schema) {
	if schema.Type != "" {
		schema.Types = []string{schema.Type}
		schema.Type = ""
	}
	if !slices.Contains(schema.Types, "null") {
		schema.Types = append(schema.Types, "null")
	}
	slices.Sort(schema.Types)
}
func optional(schema *jsonschema.Schema, key string) {
	schema.Required = slices.DeleteFunc(schema.Required, func(value string) bool { return value == key })
}

// Validate validates JSON-compatible wire data for one covered DTO. Tests use
// actual handler responses; production authorization does not call this helper.
func Validate(name string, value any) error {
	schemas, err := Schemas()
	if err != nil {
		return err
	}
	schema := schemas[name]
	if schema == nil {
		return fmt.Errorf("unregistered contract %q", name)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return err
	}
	return resolved.Validate(value)
}

func Artifacts() (map[string][]byte, error) {
	schemas, err := Schemas()
	if err != nil {
		return nil, err
	}
	bundle := &jsonschema.Schema{Schema: schemaDialect, ID: "urn:postra:contracts:v1", Title: "Postra canonical core DTO contracts (partial coverage)", Comment: "Generated by go run ./cmd/postra-contracts. Does not cover all REST/MCP endpoints or dynamic validation/ownership policies.", Defs: schemas}
	jsonSchemas, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return nil, err
	}
	openapi, err := json.MarshalIndent(openAPIDocument(schemas), "", "  ")
	if err != nil {
		return nil, err
	}
	typescript, err := typeScript(schemas)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{SchemaPath: append(jsonSchemas, '\n'), OpenAPIPath: append(openapi, '\n'), TypeScriptPath: typescript}, nil
}
