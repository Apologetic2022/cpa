package claude

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
)

// An image that only exists as a markdown data: URL never reaches the screen in
// clients that harden their markdown renderer (harden-react-markdown /
// streamdown replace it with "[Image blocked: …]"). Carrying the bytes in a
// native image content block instead takes the sanitiser out of the loop: the
// client renders the block by type rather than parsing a URL out of prose.

// openAIImageDataURLs collects the data: URLs from an OpenAI images array,
// which is where an upstream reports images it produced alongside the text.
func openAIImageDataURLs(node gjson.Result) []string {
	if !node.Exists() || !node.IsArray() {
		return nil
	}
	var urls []string
	node.ForEach(func(_, item gjson.Result) bool {
		url := strings.TrimSpace(item.Get("image_url.url").String())
		if url == "" {
			url = strings.TrimSpace(item.Get("url").String())
		}
		if strings.HasPrefix(url, "data:") {
			urls = append(urls, url)
		}
		return true
	})
	return urls
}

// anthropicImageBlock renders a data: URL as an Anthropic image content block.
func anthropicImageBlock(dataURL string) ([]byte, bool) {
	mediaType, payload, ok := splitImageDataURL(dataURL)
	if !ok {
		return nil, false
	}
	block := []byte(`{"type":"image","source":{"type":"base64","media_type":"","data":""}}`)
	block, _ = sjson.SetBytes(block, "source.media_type", mediaType)
	block, _ = sjson.SetBytes(block, "source.data", payload)
	return block, true
}

// splitImageDataURL pulls the media type and base64 payload out of a data: URL.
func splitImageDataURL(dataURL string) (mediaType, payload string, ok bool) {
	if !strings.HasPrefix(dataURL, "data:") {
		return "", "", false
	}
	comma := strings.IndexByte(dataURL, ',')
	if comma < 0 {
		return "", "", false
	}
	meta := dataURL[len("data:"):comma]
	payload = dataURL[comma+1:]
	if !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	mediaType = strings.TrimSuffix(meta, ";base64")
	if !strings.HasPrefix(mediaType, "image/") || payload == "" {
		return "", "", false
	}
	return mediaType, payload, true
}

// emitImageContentBlock announces one image as its own content block. The whole
// payload rides in content_block_start because Anthropic has no delta type for
// image data.
func emitImageContentBlock(param *ConvertOpenAIResponseToAnthropicParams, dataURL string, results *[][]byte) {
	block, ok := anthropicImageBlock(dataURL)
	if !ok {
		return
	}
	stopThinkingContentBlock(param, results)
	stopTextContentBlock(param, results)

	index := param.NextContentBlockIndex
	param.NextContentBlockIndex++
	start := []byte(`{"type":"content_block_start","index":0,"content_block":{}}`)
	start, _ = sjson.SetBytes(start, "index", index)
	start, _ = sjson.SetRawBytes(start, "content_block", block)
	*results = append(*results, translatorcommon.AppendSSEEventBytes(nil, "content_block_start", start, 2))

	stop := []byte(`{"type":"content_block_stop","index":0}`)
	stop, _ = sjson.SetBytes(stop, "index", index)
	*results = append(*results, translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", stop, 2))
}
