package cursor

import (
	"encoding/json"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

func toProtobufValue(v any) (*structpb.Value, error) {
	if v == nil {
		return structpb.NewNullValue(), nil
	}
	return structpb.NewValue(v)
}

func fromProtobufValue(v *structpb.Value) any {
	if v == nil {
		return nil
	}
	raw, err := protojson.Marshal(v)
	if err != nil {
		return v.AsInterface()
	}
	var decoded any
	if err = json.Unmarshal(raw, &decoded); err != nil {
		return v.AsInterface()
	}
	if s, ok := decoded.(string); ok {
		trimmed := trimSpaceJSON(s)
		if trimmed == "" {
			return s
		}
		var nested any
		if json.Unmarshal([]byte(trimmed), &nested) == nil {
			return nested
		}
	}
	return decoded
}

func trimSpaceJSON(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\r' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func decodeMcpArguments(args *agentv1.McpArgs) map[string]any {
	out := map[string]any{}
	if args == nil {
		return out
	}
	for k, v := range args.GetArgs() {
		out[k] = fromProtobufValue(v)
	}
	return out
}
