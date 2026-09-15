package contracts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestGeneratedContractsAreCurrentAndDeterministic(t *testing.T) {
	first, err := Artifacts()
	if err != nil {
		t.Fatal(err)
	}
	second, err := Artifacts()
	if err != nil {
		t.Fatal(err)
	}
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(here), "../../../.."))
	for path, want := range first {
		if !bytes.Equal(want, second[path]) {
			t.Fatalf("non-deterministic artifact %s", path)
		}
		actual, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil || !bytes.Equal(actual, want) {
			t.Errorf("contract drift: %s; run go run ./cmd/postra-contracts and review the diff (read error: %v)", path, err)
		}
	}
}

func TestSchemasMatchGoEncodingIncludingNilEmbeddedFields(t *testing.T) {
	for name, typ := range Registry() {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(reflect.Zero(typ).Interface())
			if err != nil {
				t.Fatal(err)
			}
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				t.Fatal(err)
			}
			if err := Validate(name, value); err != nil {
				t.Fatalf("Go wire value rejected: %s: %v", raw, err)
			}
		})
	}
}

func TestContractsExposePublicShapeAndRejectWrongTypes(t *testing.T) {
	schemas, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	if schemas["RenderMailInput"].Properties["selectionOnly"] != nil || schemas["RenderMailInput"].Properties["SignatureHTML"] != nil {
		t.Fatal("internal renderer controls leaked into public schema")
	}
	if !schemas["SettingsPatch"].Properties["secrets"].WriteOnly {
		t.Fatal("secret writes lost their write-only annotation")
	}
	if err := Validate("SettingsPatch", map[string]any{"reset": []any{"compose.template"}}); err != nil {
		t.Fatalf("valid reset-only PATCH rejected: %v", err)
	}
	if err := Validate("CreateDraftInput", map[string]any{"account_id": "acc-own", "body": "hello"}); err != nil {
		t.Fatalf("default draft kind rejected: %v", err)
	}
	for name, value := range map[string]any{
		"AskInput":        map[string]any{"question": 42},
		"RenderMailInput": map[string]any{"use_signature": "true"},
		"SettingsPatch":   map[string]any{"values": map[string]any{"compose.template": false}},
		"SettingsView":    map[string]any{"fields": "not-an-array", "revision": "1"},
	} {
		if err := Validate(name, value); err == nil {
			t.Errorf("invalid %s shape accepted", name)
		}
	}
}

func TestOpenAPIDeclaresOnlyCoveredCanonicalRoutes(t *testing.T) {
	files, err := Artifacts()
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(files[OpenAPIPath], &document); err != nil {
		t.Fatal(err)
	}
	if document["openapi"] != "3.1.0" || document["jsonSchemaDialect"] != schemaDialect {
		t.Fatal("OpenAPI/schema version mismatch")
	}
	paths := document["paths"].(map[string]any)
	if paths["/qa"] == nil || paths["/preferences"] == nil || paths["/drafts/{id}/preview"] == nil || paths["/drafts/{id}/send"] != nil {
		t.Fatal("coverage boundary changed without contract review")
	}
	components := document["components"].(map[string]any)["schemas"].(map[string]any)
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if raw, ok := value["$ref"].(string); ok {
				const prefix = "#/components/schemas/"
				if len(raw) <= len(prefix) || raw[:len(prefix)] != prefix || components[raw[len(prefix):]] == nil {
					t.Errorf("invalid OpenAPI local reference: %s", raw)
				}
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(document)
}
