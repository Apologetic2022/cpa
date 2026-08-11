package relay

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// messageAggregator assembles a stream of Anthropic SSE events into one complete
// /v1/messages response object (used by the non-stream Execute path).
type messageAggregator struct {
	header     []byte // message_start's message object (id/role/model/usage seed)
	blocks     map[int64]*blockState
	blockOrder []int64
	started    bool
	stopReason string
	stopSeq    string
	usage      []byte // latest usage node
}

type blockState struct {
	kind      string
	base      []byte // content_block_start payload (minus accumulated fields)
	text      strings.Builder
	thinking  strings.Builder
	signature string
	partial   strings.Builder // input_json_delta accumulation
}

func newMessageAggregator() *messageAggregator {
	return &messageAggregator{blocks: make(map[int64]*blockState)}
}

func (a *messageAggregator) add(event string, data []byte) error {
	if !gjson.ValidBytes(data) {
		return nil // ignore non-JSON frames (keep-alives etc.)
	}
	switch event {
	case "message_start":
		msg := gjson.GetBytes(data, "message")
		if !msg.Exists() {
			return fmt.Errorf("message_start without message object")
		}
		a.header = []byte(msg.Raw)
		a.started = true
		if u := msg.Get("usage"); u.Exists() {
			a.usage = []byte(u.Raw)
		}
	case "content_block_start":
		idx := gjson.GetBytes(data, "index").Int()
		cb := gjson.GetBytes(data, "content_block")
		if !cb.Exists() {
			return fmt.Errorf("content_block_start without content_block")
		}
		st := &blockState{kind: cb.Get("type").String(), base: []byte(cb.Raw)}
		a.blocks[idx] = st
		a.blockOrder = append(a.blockOrder, idx)
	case "content_block_delta":
		idx := gjson.GetBytes(data, "index").Int()
		st, ok := a.blocks[idx]
		if !ok {
			return nil
		}
		delta := gjson.GetBytes(data, "delta")
		switch delta.Get("type").String() {
		case "text_delta":
			st.text.WriteString(delta.Get("text").String())
		case "thinking_delta":
			st.thinking.WriteString(delta.Get("thinking").String())
		case "signature_delta":
			st.signature = delta.Get("signature").String()
		case "input_json_delta":
			st.partial.WriteString(delta.Get("partial_json").String())
		}
	case "message_delta":
		if d := gjson.GetBytes(data, "delta"); d.Exists() {
			if sr := d.Get("stop_reason"); sr.Exists() && sr.Type != gjson.Null {
				a.stopReason = sr.String()
			}
			if ss := d.Get("stop_sequence"); ss.Exists() && ss.Type != gjson.Null {
				a.stopSeq = ss.String()
			}
		}
		if u := gjson.GetBytes(data, "usage"); u.Exists() {
			a.usage = []byte(u.Raw)
		}
	}
	return nil
}

// message renders the aggregated full /v1/messages response JSON.
func (a *messageAggregator) message() ([]byte, error) {
	if !a.started {
		return nil, fmt.Errorf("stream ended before message_start; cannot aggregate a response")
	}
	out := a.header

	content := make([]json.RawMessage, 0, len(a.blockOrder))
	for _, idx := range a.blockOrder {
		st := a.blocks[idx]
		var raw []byte
		var err error
		switch st.kind {
		case "text":
			raw, err = sjson.SetBytes(st.base, "text", st.text.String())
		case "thinking":
			raw, err = sjson.SetBytes(st.base, "thinking", st.thinking.String())
			if err == nil && st.signature != "" {
				raw, err = sjson.SetBytes(raw, "signature", st.signature)
			}
		case "tool_use":
			input := json.RawMessage(`{}`)
			if st.partial.Len() > 0 {
				if gjson.Valid(st.partial.String()) {
					input = json.RawMessage(st.partial.String())
				} else {
					// Turn aborted mid-JSON: degrade gracefully to empty input rather
					// than emit invalid JSON.
					input = json.RawMessage(`{}`)
				}
			}
			raw, err = sjson.SetRawBytes(st.base, "input", input)
		default:
			raw = st.base
		}
		if err != nil {
			return nil, fmt.Errorf("assemble content block %d: %w", idx, err)
		}
		content = append(content, json.RawMessage(raw))
	}
	contentRaw, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	if out, err = sjson.SetRawBytes(out, "content", contentRaw); err != nil {
		return nil, err
	}
	if a.stopReason != "" {
		if out, err = sjson.SetBytes(out, "stop_reason", a.stopReason); err != nil {
			return nil, err
		}
	}
	if a.stopSeq != "" {
		if out, err = sjson.SetBytes(out, "stop_sequence", a.stopSeq); err != nil {
			return nil, err
		}
	}
	if a.usage != nil {
		if out, err = sjson.SetRawBytes(out, "usage", a.usage); err != nil {
			return nil, err
		}
	}
	return out, nil
}
