package cursor

import (
	"strings"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
)

// ModelParameter is one Cursor RequestedModel.parameters entry.
type ModelParameter struct {
	ID    string
	Value string
}

// ModelSelection is the wire model id + catalog-style parameters for AgentRun.
type ModelSelection struct {
	ModelID              string
	PublicID             string
	MaxMode              bool
	Parameters           []ModelParameter
	VariantStringRepr    bool
}

var cursorSuffixTokens = map[string]struct{}{
	"thinking": {},
	"fast":     {},
	"low":      {},
	"medium":   {},
	"high":     {},
	"xhigh":    {},
	"max":      {},
}

// ResolveRequestedModel maps a public/legacy Cursor model slug onto the base
// model id plus RequestedModel.parameters, matching cursor2api 3.7.12 behavior:
//
//   - Public IDs are base names (no -thinking / -xhigh suffix)
//   - Desktop default prefers thinking=true when no variant suffix is given
//   - Hyphen suffixes are resolved into exact parameter id/value pairs
func ResolveRequestedModel(requested string) ModelSelection {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		requested = "default"
	}
	base, tokens := splitCursorModelSuffix(requested)
	sel := ModelSelection{ModelID: base, PublicID: base}
	if len(tokens) == 0 {
		// Only attach catalog-advertised parameters. Do not invent thinking=true
		// when AvailableModels has not validated a matching variant.
		if entry, ok := catalogEntry(base); ok {
			sel.MaxMode = entry.MaxMode
			if len(entry.Parameters) > 0 {
				sel.Parameters = append([]ModelParameter(nil), entry.Parameters...)
			}
			if wire := strings.TrimSpace(entry.WireID); wire != "" && !strings.EqualFold(wire, base) {
				sel.ModelID = wire
				sel.VariantStringRepr = true
				sel.Parameters = nil // encoded in the variant string id
			}
		}
		return sel
	}

	seen := map[string]string{}
	set := func(id, value string) {
		if id == "" || value == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = value
		sel.Parameters = append(sel.Parameters, ModelParameter{ID: id, Value: value})
	}

	for _, token := range tokens {
		switch token {
		case "thinking":
			set("thinking", "true")
		case "fast":
			set("fast", "true")
		case "low", "medium", "high", "xhigh":
			// Cursor has used both "reasoning" and "effort" across vendors.
			set("reasoning", token)
			set("effort", token)
		case "max":
			sel.MaxMode = true
		}
	}
	if entry, ok := catalogEntry(base); ok {
		tmp := entry
		tmp.Parameters = sel.Parameters
		if wire := matchUsableWireID(tmp, cachedUsableModels()); wire != "" {
			sel.ModelID = wire
			sel.VariantStringRepr = true
			sel.Parameters = nil
			return sel
		}
		if wire := strings.TrimSpace(entry.WireID); wire != "" {
			sel.ModelID = wire
			sel.VariantStringRepr = true
			sel.Parameters = nil
		}
	}
	return sel
}

func splitCursorModelSuffix(requested string) (base string, tokens []string) {
	parts := strings.Split(requested, "-")
	if len(parts) <= 1 {
		return requested, nil
	}
	cut := len(parts)
	for cut > 1 {
		token := strings.ToLower(parts[cut-1])
		if _, ok := cursorSuffixTokens[token]; !ok {
			break
		}
		cut--
	}
	if cut == len(parts) {
		return requested, nil
	}
	base = strings.Join(parts[:cut], "-")
	if strings.TrimSpace(base) == "" {
		return requested, nil
	}
	rawTokens := parts[cut:]
	tokens = make([]string, 0, len(rawTokens))
	for _, part := range rawTokens {
		tokens = append(tokens, strings.ToLower(part))
	}
	return base, tokens
}

func toRequestedModelProto(sel ModelSelection) *agentv1.RequestedModel {
	out := &agentv1.RequestedModel{
		ModelId:                       sel.ModelID,
		MaxMode:                       sel.MaxMode,
		IsVariantStringRepresentation: sel.VariantStringRepr,
	}
	if sel.VariantStringRepr {
		return out
	}
	for _, p := range sel.Parameters {
		id := strings.TrimSpace(p.ID)
		value := strings.TrimSpace(p.Value)
		if id == "" || value == "" {
			continue
		}
		out.Parameters = append(out.Parameters, &agentv1.RequestedModel_ModelParameterValue{
			Id:    id,
			Value: value,
		})
	}
	return out
}
