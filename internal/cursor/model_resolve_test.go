package cursor

import "testing"

func TestResolveRequestedModelUsesCatalogParams(t *testing.T) {
	RememberCatalog([]CatalogModel{{
		ID:         "claude-4.6-sonnet",
		Parameters: []ModelParameter{{ID: "thinking", Value: "true"}},
	}})
	sel := ResolveRequestedModel("claude-4.6-sonnet")
	if sel.PublicID != "claude-4.6-sonnet" {
		t.Fatalf("public id = %q", sel.PublicID)
	}
	if len(sel.Parameters) != 1 || sel.Parameters[0].ID != "thinking" || sel.Parameters[0].Value != "true" {
		t.Fatalf("expected catalog thinking=true, got %#v", sel.Parameters)
	}
}

func TestResolveRequestedModelUsesWireID(t *testing.T) {
	RememberCatalog([]CatalogModel{{
		ID:         "grok-4.5",
		WireID:     "cursor-grok-4.5-high-fast",
		Parameters: []ModelParameter{{ID: "effort", Value: "high"}, {ID: "fast", Value: "true"}},
	}})
	sel := ResolveRequestedModel("grok-4.5")
	if sel.ModelID != "cursor-grok-4.5-high-fast" || !sel.VariantStringRepr {
		t.Fatalf("expected wire variant id, got %#v", sel)
	}
	if len(sel.Parameters) != 0 {
		t.Fatalf("variant string should not also send parameters: %#v", sel.Parameters)
	}
}

func TestResolveRequestedModelDefaultSelector(t *testing.T) {
	sel := ResolveRequestedModel("default")
	if sel.ModelID != "default" || len(sel.Parameters) != 0 {
		t.Fatalf("default selector should have no parameters, got %#v", sel)
	}
}

func TestResolveRequestedModelSuffix(t *testing.T) {
	sel := ResolveRequestedModel("claude-4.6-sonnet-thinking-xhigh")
	if sel.ModelID != "claude-4.6-sonnet" {
		t.Fatalf("base model = %q", sel.ModelID)
	}
	got := map[string]string{}
	for _, p := range sel.Parameters {
		got[p.ID] = p.Value
	}
	if got["thinking"] != "true" || got["reasoning"] != "xhigh" || got["effort"] != "xhigh" {
		t.Fatalf("unexpected parameters: %#v", sel.Parameters)
	}
}

func TestBuildRunRequestSendsParameters(t *testing.T) {
	msg, _, _, err := buildRunRequest("gpt-5.4-thinking", []ChatMessage{
		{Role: "user", Content: "hi"},
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	run := msg.GetRunRequest()
	req := run.GetRequestedModel()
	if req.GetModelId() != "gpt-5.4" {
		t.Fatalf("requested model id = %q", req.GetModelId())
	}
	if len(req.GetParameters()) == 0 || req.GetParameters()[0].GetId() != "thinking" {
		t.Fatalf("expected thinking parameter, got %#v", req.GetParameters())
	}
}
