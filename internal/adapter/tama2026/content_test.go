package tama2026

import (
	"encoding/json"
	"testing"
)

// TestNormalizeCompleteResultValidatesContentBlockShapes pins the
// protocol-version wire validation the contract assigns to the endpoint
// adapter: the required fields of the known 2026 content block types are
// enforced before a result can be persisted, while unknown types are
// preserved as extension blocks.
func TestNormalizeCompleteResultValidatesContentBlockShapes(t *testing.T) {
	envelope := func(content string) string {
		return `{"resultType":"complete","isError":false,"content":[` + content + `]}`
	}
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{name: "text requires text", content: `{"type":"text"}`, wantErr: true},
		{name: "text field must be a string", content: `{"type":"text","text":5}`, wantErr: true},
		{name: "image requires data and mimeType", content: `{"type":"image","data":"aGk="}`, wantErr: true},
		{name: "audio requires data and mimeType", content: `{"type":"audio","mimeType":"audio/wav"}`, wantErr: true},
		{name: "resource_link requires uri and name", content: `{"type":"resource_link","uri":"file:///x"}`, wantErr: true},
		{name: "embedded resource requires a resource object", content: `{"type":"resource"}`, wantErr: true},
		{name: "embedded resource requires text or blob", content: `{"type":"resource","resource":{"uri":"file:///x"}}`, wantErr: true},
		{name: "valid text", content: `{"type":"text","text":"done"}`},
		{name: "valid image", content: `{"type":"image","data":"aGk=","mimeType":"image/png"}`},
		{name: "valid audio", content: `{"type":"audio","data":"aGk=","mimeType":"audio/wav"}`},
		{name: "valid resource_link", content: `{"type":"resource_link","uri":"file:///x","name":"x"}`},
		{name: "valid embedded resource", content: `{"type":"resource","resource":{"uri":"file:///x","text":"body"}}`},
		{name: "unknown type is an extension block", content: `{"type":"x-custom","payload":42}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeCompleteResult(json.RawMessage(envelope(tt.content)))
			if (err != nil) != tt.wantErr {
				t.Fatalf("NormalizeCompleteResult = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
