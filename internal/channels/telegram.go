package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/local/picobot/internal/chat"
)

var (
	draftCounter int64
	statesMu     sync.Mutex
	states       = make(map[string]*chatState)
)

type chatState struct {
	draftID         int64
	accumulatedText string
}

// StartTelegram is a convenience wrapper that uses the real polling implementation
// with the standard Telegram base URL.
// allowFrom is a list of Telegram user IDs permitted to interact with the bot.
// If empty, ALL users are allowed (open mode).
func StartTelegram(ctx context.Context, hub *chat.Hub, token string, allowFrom []string) error {
	if token == "" {
		return fmt.Errorf("telegram token not provided")
	}
	base := "https://api.telegram.org/bot" + token
	return StartTelegramWithBase(ctx, hub, token, base, allowFrom)
}

// StartTelegramWithBase starts long-polling against the given base URL (e.g., https://api.telegram.org/bot<TOKEN> or a test server URL).
// allowFrom restricts which Telegram user IDs may send messages. Empty means allow all.
func StartTelegramWithBase(ctx context.Context, hub *chat.Hub, token, base string, allowFrom []string) error {
	if base == "" {
		return fmt.Errorf("base URL is required")
	}

	// Build a fast lookup set for allowed user IDs.
	allowed := make(map[string]struct{}, len(allowFrom))
	for _, id := range allowFrom {
		allowed[id] = struct{}{}
	}

	client := &http.Client{Timeout: 45 * time.Second}

	// inbound polling goroutine
	go func() {
		offset := int64(0)
		for {
			select {
			case <-ctx.Done():
				log.Println("telegram: stopping inbound polling")
				return
			default:
			}

			values := url.Values{}
			values.Set("offset", strconv.FormatInt(offset, 10))
			values.Set("timeout", "30")
			u := base + "/getUpdates"
			resp, err := client.PostForm(u, values)
			if err != nil {
				log.Printf("telegram getUpdates error: %v", err)
				time.Sleep(1 * time.Second)
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var gu struct {
				Ok     bool `json:"ok"`
				Result []struct {
					UpdateID int64 `json:"update_id"`
					Message  *struct {
						MessageID int64 `json:"message_id"`
						From      *struct {
							ID int64 `json:"id"`
						} `json:"from"`
						Chat struct {
							ID int64 `json:"id"`
						} `json:"chat"`
						Text string `json:"text"`
					} `json:"message"`
				} `json:"result"`
			}
			if err := json.Unmarshal(body, &gu); err != nil {
				log.Printf("telegram: invalid getUpdates response: %v", err)
				continue
			}
			for _, upd := range gu.Result {
				if upd.UpdateID >= offset {
					offset = upd.UpdateID + 1
				}
				if upd.Message == nil {
					continue
				}
				m := upd.Message
				fromID := ""
				if m.From != nil {
					fromID = strconv.FormatInt(m.From.ID, 10)
				}
				// Enforce allowFrom: if the list is non-empty, reject unknown senders.
				if len(allowed) > 0 {
					if _, ok := allowed[fromID]; !ok {
						log.Printf("telegram: dropping message from unauthorized user %s", fromID)
						continue
					}
				}
				chatID := strconv.FormatInt(m.Chat.ID, 10)

				// Start a new turn: increment draft counter and assign it
				newDraftID := atomic.AddInt64(&draftCounter, 1)
				statesMu.Lock()
				states[chatID] = &chatState{
					draftID:         newDraftID,
					accumulatedText: "",
				}
				statesMu.Unlock()

				// Send an initial draft to indicate thinking / typing state
				go func(cID string, dID int64) {
					uDraft := base + "/sendMessageDraft"
					vDraft := url.Values{}
					vDraft.Set("chat_id", cID)
					vDraft.Set("draft_id", strconv.FormatInt(dID, 10))
					vDraft.Set("text", "") // empty text shows "Thinking..." placeholder
					respDraft, errDraft := client.PostForm(uDraft, vDraft)
					if errDraft == nil {
						io.ReadAll(respDraft.Body)
						respDraft.Body.Close()
					}
				}(chatID, newDraftID)

				hub.In <- chat.Inbound{
					Channel:   "telegram",
					SenderID:  fromID,
					ChatID:    chatID,
					Content:   m.Text,
					Timestamp: time.Now(),
				}
			}
		}
	}()

	// Subscribe to the outbound queue before launching the goroutine so the
	// registration is visible to the hub router from the moment this function returns.
	outCh := hub.Subscribe("telegram")

	// outbound sender goroutine
	go func() {
		client := &http.Client{Timeout: 10 * time.Second}
		for {
			select {
			case <-ctx.Done():
				log.Println("telegram: stopping outbound sender")
				return
			case out := <-outCh:
				statesMu.Lock()
				state, ok := states[out.ChatID]
				if !ok {
					// Fallback if no draft is initialized
					newDraftID := atomic.AddInt64(&draftCounter, 1)
					state = &chatState{
						draftID:         newDraftID,
						accumulatedText: "",
					}
					states[out.ChatID] = state
				}
				statesMu.Unlock()

				isNotif := false
				if out.Metadata != nil {
					if v, ok := out.Metadata["is_notification"].(bool); ok && v {
						isNotif = true
					}
				}

				if isNotif {
					// Accumulate intermediate notification status messages
					if state.accumulatedText != "" {
						state.accumulatedText += "\n" + out.Content
					} else {
						state.accumulatedText = out.Content
					}

					u := base + "/sendMessageDraft"
					v := url.Values{}
					v.Set("chat_id", out.ChatID)
					v.Set("draft_id", strconv.FormatInt(state.draftID, 10))
					v.Set("text", markdownToHTML(state.accumulatedText))
					v.Set("parse_mode", "HTML")
					resp, err := client.PostForm(u, v)
					if err != nil {
						log.Printf("telegram sendMessageDraft error: %v", err)
						continue
					}
					body, readErr := io.ReadAll(resp.Body)
					resp.Body.Close()
					if readErr == nil {
						var res struct {
							Ok          bool   `json:"ok"`
							Description string `json:"description"`
						}
						if json.Unmarshal(body, &res) == nil && !res.Ok {
							log.Printf("telegram sendMessageDraft HTML parse failed: %s. Falling back to plain text.", res.Description)
							v.Del("parse_mode")
							v.Set("text", state.accumulatedText)
							resp2, err2 := client.PostForm(u, v)
							if err2 == nil {
								io.ReadAll(resp2.Body)
								resp2.Body.Close()
							}
						}
					}
				} else {
					// Final message: send via standard sendMessage to finalize and replace the draft
					u := base + "/sendMessage"
					v := url.Values{}
					v.Set("chat_id", out.ChatID)
					v.Set("text", markdownToHTML(out.Content))
					v.Set("parse_mode", "HTML")
					resp, err := client.PostForm(u, v)
					if err != nil {
						log.Printf("telegram sendMessage error: %v", err)
						continue
					}
					body, readErr := io.ReadAll(resp.Body)
					resp.Body.Close()
					if readErr == nil {
						var res struct {
							Ok          bool   `json:"ok"`
							Description string `json:"description"`
						}
						if json.Unmarshal(body, &res) == nil && !res.Ok {
							log.Printf("telegram sendMessage HTML parse failed: %s. Falling back to plain text.", res.Description)
							v.Del("parse_mode")
							v.Set("text", out.Content)
							resp2, err2 := client.PostForm(u, v)
							if err2 == nil {
								io.ReadAll(resp2.Body)
								resp2.Body.Close()
							}
						}
					}
				}
			}
		}
	}()

	return nil
}
