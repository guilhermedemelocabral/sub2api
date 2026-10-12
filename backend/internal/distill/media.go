package distill

import (
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

func OmitMessage(msg apicompat.ChatMessage) apicompat.ChatMessage {
	msg.Content = omitContent(msg.Content)
	return msg
}

func omitContent(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed[0] != '[' {
		return raw
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		return raw
	}
	changed := false
	for i, part := range parts {
		typeName, _ := part["type"].(string)
		if url := nestedString(part, "image_url", "url"); strings.HasPrefix(url, "data:") {
			parts[i] = marker(typeName, len(url))
			changed = true
			continue
		}
		if data := nestedString(part, "file", "file_data"); strings.HasPrefix(data, "data:") {
			parts[i] = marker(typeName, len(data))
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(parts)
	if err != nil {
		return raw
	}
	return out
}

func nestedString(part map[string]any, objectKey, field string) string {
	object, _ := part[objectKey].(map[string]any)
	value, _ := object[field].(string)
	return value
}

func marker(typeName string, n int) map[string]any {
	if typeName == "" {
		typeName = "media"
	}
	return map[string]any{"type": typeName, "omitted": true, "bytes": n}
}
