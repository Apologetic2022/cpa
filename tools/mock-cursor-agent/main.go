// Command mock-cursor-agent is a local stand-in for Cursor's Agent Connect
// upstream (api2.cursor.sh). It speaks just enough of the bidi Run protocol
// to exercise the gateway's GenerateImage pipeline end-to-end without real
// Cursor credentials:
//
//	client run_request -> server interaction_query (GenerateImageRequestQuery)
//	client interaction_response (auto-approval) -> server tool_call_completed
//	with base64 image data -> turn_ended -> Connect end-stream frame.
//
// Point a cursor auth file's base_url at this server (TLS + HTTP/2 required,
// the CA must be trusted by the system) and every image request succeeds with
// a generated PNG.
package main

import (
	"bytes"
	"encoding/base64"
	"flag"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9443", "listen address")
	cert := flag.String("cert", "server.crt", "TLS certificate (SAN must cover the base_url host)")
	key := flag.String("key", "server.key", "TLS private key")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/agent.v1.AgentService/Run", handleRun)
	log.Printf("mock cursor agent listening on https://%s", *addr)
	server := &http.Server{Addr: *addr, Handler: mux}
	log.Fatal(server.ListenAndServeTLS(*cert, *key))
}

func handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	log.Printf("run opened: %s %s proto=%s", r.Method, r.URL.Path, r.Proto)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/connect+proto")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	send := func(msg *agentv1.AgentServerMessage) bool {
		payload, err := proto.Marshal(msg)
		if err != nil {
			log.Printf("marshal server message: %v", err)
			return false
		}
		if _, err = w.Write(cursorlib.EncodeEnvelope(payload, false)); err != nil {
			log.Printf("write server message: %v", err)
			return false
		}
		flusher.Flush()
		return true
	}

	decoder := cursorlib.NewDecoder()
	buf := make([]byte, 32*1024)
	sentQuery := false
	for {
		n, errRead := r.Body.Read(buf)
		if n > 0 {
			envelopes, errFeed := decoder.Feed(buf[:n])
			if errFeed != nil {
				log.Printf("decode client envelope: %v", errFeed)
				return
			}
			for _, env := range envelopes {
				if env.EndStream() {
					log.Printf("client closed stream")
					return
				}
				clientMsg := &agentv1.AgentClientMessage{}
				if err := proto.Unmarshal(env.Payload, clientMsg); err != nil {
					log.Printf("decode client message: %v", err)
					continue
				}
				switch m := clientMsg.Message.(type) {
				case *agentv1.AgentClientMessage_RunRequest:
					if sentQuery {
						continue
					}
					sentQuery = true
					log.Printf("run_request received; sending GenerateImage interaction query")
					if !send(&agentv1.AgentServerMessage{
						Message: &agentv1.AgentServerMessage_InteractionQuery{
							InteractionQuery: &agentv1.InteractionQuery{
								Id: 1,
								Query: &agentv1.InteractionQuery_GenerateImageRequestQuery{
									GenerateImageRequestQuery: &agentv1.GenerateImageRequestQuery{
										Args:       &agentv1.GenerateImageArgs{Description: "mock image"},
										ToolCallId: "mock-image-call-1",
									},
								},
							},
						},
					}) {
						return
					}
				case *agentv1.AgentClientMessage_InteractionResponse:
					approved := m.InteractionResponse.GetGenerateImageRequestResponse().GetApproved()
					if approved == nil {
						log.Printf("image request rejected by client; ending run")
						finishRun(w, flusher, send, "", "")
						return
					}
					log.Printf("approval received (description=%q); returning generated image", approved.GetDescription())
					finishRun(w, flusher, send, approved.GetDescription(), renderPNGBase64(approved.GetDescription()))
					return
				case *agentv1.AgentClientMessage_ClientHeartbeat:
					// keepalive, ignore
				default:
					log.Printf("ignoring client message %T", m)
				}
			}
		}
		if errRead != nil {
			log.Printf("client body read ended: %v", errRead)
			return
		}
	}
}

// finishRun emits the post-approval server sequence: optional image result,
// a text delta, turn_ended usage, and the Connect end-stream trailer.
func finishRun(w http.ResponseWriter, flusher http.Flusher, send func(*agentv1.AgentServerMessage) bool, description, imageB64 string) {
	if imageB64 != "" {
		if !send(&agentv1.AgentServerMessage{
			Message: &agentv1.AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &agentv1.InteractionUpdate{
					Message: &agentv1.InteractionUpdate_ToolCallCompleted{
						ToolCallCompleted: &agentv1.ToolCallCompletedUpdate{
							CallId: "mock-image-call-1",
							ToolCall: &agentv1.ToolCall{
								Tool: &agentv1.ToolCall_GenerateImageToolCall{
									GenerateImageToolCall: &agentv1.GenerateImageToolCall{
										Args: &agentv1.GenerateImageArgs{Description: description},
										Result: &agentv1.GenerateImageResult{
											Result: &agentv1.GenerateImageResult_Success{
												Success: &agentv1.GenerateImageSuccess{
													FilePath:  "assets/mock-image.png",
													ImageData: imageB64,
												},
											},
										},
									},
								},
								ToolCallId: "mock-image-call-1",
							},
						},
					},
				},
			},
		}) {
			return
		}
	}
	if !send(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_TextDelta{
					TextDelta: &agentv1.TextDeltaUpdate{Text: "Generated 1 image."},
				},
			},
		},
	}) {
		return
	}
	inputTokens, outputTokens := int64(42), int64(7)
	if !send(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_TurnEnded{
					TurnEnded: &agentv1.TurnEndedUpdate{
						InputTokens:  &inputTokens,
						OutputTokens: &outputTokens,
					},
				},
			},
		},
	}) {
		return
	}
	if _, err := w.Write(cursorlib.EncodeEnvelope([]byte("{}"), true)); err != nil {
		log.Printf("write end-stream frame: %v", err)
		return
	}
	flusher.Flush()
}

// renderPNGBase64 draws a 256x256 gradient PNG so the returned bytes are a
// real decodable image rather than a canned fixture.
func renderPNGBase64(description string) string {
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	seed := 0
	for _, r := range description {
		seed += int(r)
	}
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8((x + seed) % 256),
				G: uint8((y + seed/2) % 256),
				B: uint8((x + y) / 2),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		log.Printf("encode png: %v", err)
		return ""
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
