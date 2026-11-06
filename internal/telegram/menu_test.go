package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	tele "gopkg.in/telebot.v4"
)

func openBotMenuScreen(t *testing.T, screen string) (*Router, Action, string) {
	t.Helper()
	router := NewRouter(nil, RouterOptions{AllowedUsers: []int64{42}})
	actions, err := router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat, Command: CommandRoot})
	if err != nil || len(actions) != 1 {
		t.Fatalf("open menu: actions=%v err=%v", actions, err)
	}
	data := buttonData(actions[0].Keyboard, screen)
	actions, err = router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat, MessageID: 100, CallbackData: data})
	if err != nil || len(actions) != 1 {
		t.Fatalf("open screen: actions=%v err=%v", actions, err)
	}
	return router, actions[0], data
}

func TestBotPromptReplyEditsOriginalMenu(t *testing.T) {
	for _, test := range []struct{ screen, prompt, reply string }{
		{"Configuration", "Custom timezone", "UTC"},
		{"Alert hours", "Start:", "08:00"},
	} {
		t.Run(test.prompt, func(t *testing.T) {
			router, menu, _ := openBotMenuScreen(t, test.screen)
			if test.screen == "Alert hours" {
				actions, err := router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat, MessageID: 100,
					CallbackData: buttonData(menu.Keyboard, "Custom")})
				if err != nil || len(actions) != 1 {
					t.Fatalf("select custom hours: actions=%v err=%v", actions, err)
				}
				menu = actions[0]
			}
			prompt, err := router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat, MessageID: 100,
				CallbackData: buttonData(menu.Keyboard, test.prompt)})
			if err != nil {
				t.Fatal(err)
			}
			router.CompletePromptSend(42, prompt[0].PromptID, 500)
			edits, sends := 0, 0
			bot := newLifecycleBot(t, router, func(request *http.Request) (*http.Response, error) {
				if strings.HasSuffix(request.URL.Path, "/editMessageText") {
					edits++
					var payload map[string]string
					if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					if payload["message_id"] != "100" || payload["chat_id"] != "42" {
						t.Fatalf("edit targeted a different message: %v", payload)
					}
				}
				if strings.HasSuffix(request.URL.Path, "/sendMessage") {
					sends++
				}
				return testTelegramResponse(request), nil
			})
			c := bot.bot.NewContext(tele.Update{Message: &tele.Message{ID: 501, Sender: &tele.User{ID: 42},
				Chat: &tele.Chat{ID: 42, Type: tele.ChatPrivate}, Text: test.reply, ReplyTo: &tele.Message{ID: 500}}})
			if err := bot.handleText(t.Context(), c); err != nil {
				t.Fatal(err)
			}
			if edits != 1 || sends != 0 {
				t.Fatalf("edits=%d sends=%d, want original menu edit only", edits, sends)
			}
		})
	}
}

func TestBotUnchangedMenuDoesNotSendDuplicate(t *testing.T) {
	for _, unchanged := range []error{tele.ErrMessageNotModified, tele.ErrSameMessageContent} {
		t.Run(unchanged.Error(), func(t *testing.T) {
			router, menu, _ := openBotMenuScreen(t, "Alert hours")
			sends := 0
			bot := newLifecycleBot(t, router, func(request *http.Request) (*http.Response, error) {
				if strings.HasSuffix(request.URL.Path, "/editMessageText") {
					return nil, unchanged
				}
				if strings.HasSuffix(request.URL.Path, "/sendMessage") {
					sends++
				}
				return testTelegramResponse(request), nil
			})
			if err := bot.handleCallback(t.Context(), bot.bot.NewContext(callbackUpdate(buttonData(menu.Keyboard, "Peak hours")))); err != nil {
				t.Fatal(err)
			}
			if sends != 0 {
				t.Fatalf("unchanged menu sent %d duplicates", sends)
			}
		})
	}
}

func TestConcurrentPromptAndScreenActions(t *testing.T) {
	router, menu, screenData := openBotMenuScreen(t, "Configuration")
	promptData := buttonData(menu.Keyboard, "Custom timezone")
	var wg sync.WaitGroup
	for _, data := range []string{promptData, screenData} {
		wg.Go(func() {
			for range 1000 {
				if _, err := router.Handle(context.Background(), Update{UserID: 42, ChatType: PrivateChat,
					MessageID: 100, CallbackData: data}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
}

func TestBotStaleMenuPreservesCurrentPrompt(t *testing.T) {
	router, oldMenu, _ := openBotMenuScreen(t, "Configuration")
	current, err := router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat, CallbackData: "tfl:reload"})
	if err != nil {
		t.Fatal(err)
	}
	current, err = router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat,
		CallbackData: buttonData(current[0].Keyboard, "Configuration")})
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat,
		CallbackData: buttonData(current[0].Keyboard, "Custom timezone")})
	if err != nil {
		t.Fatal(err)
	}
	router.CompletePromptSend(42, prompt[0].PromptID, 502)
	if _, err := router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat,
		CallbackData: buttonData(oldMenu.Keyboard, "Custom timezone")}); err != nil {
		t.Fatal(err)
	}
	actions, err := router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat, Text: "UTC", ReplyToMessageID: 502})
	if err != nil || len(actions) != 1 || !strings.Contains(actions[0].Text, "UTC") {
		t.Fatalf("current reply was lost after stale menu: actions=%+v err=%v", actions, err)
	}
}

func TestBotLatePromptSendDoesNotReplaceLatestPrompt(t *testing.T) {
	router, menu, _ := openBotMenuScreen(t, "Alert hours")
	actions, err := router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat,
		CallbackData: buttonData(menu.Keyboard, "Custom")})
	if err != nil {
		t.Fatal(err)
	}
	menu = actions[0]
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	bot := newLifecycleBot(t, router, func(request *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(request.URL.Path, "/sendMessage") {
			return testTelegramResponse(request), nil
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		id := 502
		if strings.Contains(string(body), "Current start") {
			close(started)
			<-release
			id = 501
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: request,
			Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"ok":true,"result":{"message_id":%d,"chat":{"id":42,"type":"private"}}}`, id)))}, nil
	})
	done := make(chan error, 1)
	go func() {
		done <- bot.handleCallback(t.Context(), bot.bot.NewContext(callbackUpdate(buttonData(menu.Keyboard, "Start:"))))
	}()
	<-started
	if err := bot.handleCallback(t.Context(), bot.bot.NewContext(callbackUpdate(buttonData(menu.Keyboard, "End:")))); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	actions, err = router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat, Text: "20:00", ReplyToMessageID: 502})
	if err != nil || len(actions) != 1 || !strings.Contains(actions[0].Text, "20:00") {
		t.Fatalf("latest prompt reply was lost: actions=%+v err=%v", actions, err)
	}
}

func TestPromptCompletionOnlyChangesItsOwnSendAttempt(t *testing.T) {
	for _, replacement := range []string{"same field", "invalid reply", "reload"} {
		for _, lateMessageID := range []int64{501, 0} {
			t.Run(fmt.Sprintf("%s/late_%d", replacement, lateMessageID), func(t *testing.T) {
				router, menu, _ := openBotMenuScreen(t, "Configuration")
				old, err := router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat,
					CallbackData: buttonData(menu.Keyboard, "Custom timezone")})
				if err != nil || len(old) != 1 {
					t.Fatalf("old prompt: %v, %v", old, err)
				}
				router.CompletePromptSend(42, old[0].PromptID, 500)
				update := Update{UserID: 42, ChatType: PrivateChat,
					CallbackData: buttonData(menu.Keyboard, "Custom timezone")}
				if replacement == "invalid reply" {
					update = Update{UserID: 42, ChatType: PrivateChat, Text: "invalid-zone", ReplyToMessageID: 500}
				}
				if replacement == "reload" {
					current, err := router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat, CallbackData: "tfl:reload"})
					if err != nil || len(current) != 1 {
						t.Fatalf("reload: %v, %v", current, err)
					}
					current, err = router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat,
						CallbackData: buttonData(current[0].Keyboard, "Configuration")})
					if err != nil || len(current) != 1 {
						t.Fatalf("configuration: %v, %v", current, err)
					}
					update.CallbackData = buttonData(current[0].Keyboard, "Custom timezone")
				}
				current, err := router.Handle(t.Context(), update)
				if err != nil || len(current) != 1 || current[0].PromptID <= old[0].PromptID {
					t.Fatalf("replacement has no new identity: %v, %v", current, err)
				}
				router.CompletePromptSend(42, old[0].PromptID, lateMessageID)
				for _, replyTo := range []int64{0, 500, 501} {
					actions, err := router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat, Text: "UTC", ReplyToMessageID: replyTo})
					if err != nil || len(actions) != 0 {
						t.Fatalf("unacknowledged prompt accepted reply to %d: %v, %v", replyTo, actions, err)
					}
				}
				router.CompletePromptSend(42, current[0].PromptID, 502)
				actions, err := router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat, Text: "UTC", ReplyToMessageID: 502})
				if err != nil || len(actions) != 1 || !strings.Contains(actions[0].Text, "UTC") {
					t.Fatalf("current prompt was changed by old completion: %v, %v", actions, err)
				}
			})
		}
	}
}

func TestPromptCompletionDoesNotRestoreFailedOrExpiredPrompts(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired_%v", expired), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				router, menu, _ := openBotMenuScreen(t, "Configuration")
				prompt, err := router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat,
					CallbackData: buttonData(menu.Keyboard, "Custom timezone")})
				if err != nil || len(prompt) != 1 {
					t.Fatalf("prompt: %v, %v", prompt, err)
				}
				if expired {
					time.Sleep(DraftLifetime)
				} else {
					router.CompletePromptSend(42, prompt[0].PromptID, 0)
				}
				router.CompletePromptSend(42, prompt[0].PromptID, 500)
				actions, err := router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat, Text: "UTC", ReplyToMessageID: 500})
				if err != nil || len(actions) != 0 {
					t.Fatalf("inactive prompt accepted a reply: %v, %v", actions, err)
				}
				actions, err = router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat, Text: "/help"})
				if err != nil || len(actions) != 1 || !strings.Contains(actions[0].Text, "Use /tfl") {
					t.Fatalf("inactive prompt swallowed a command: %v, %v", actions, err)
				}
			})
		})
	}
}

func TestBotFailedPromptSendLeavesNoReplyRoute(t *testing.T) {
	for _, failure := range []string{"transport error", "missing message reference"} {
		t.Run(failure, func(t *testing.T) {
			router, menu, _ := openBotMenuScreen(t, "Configuration")
			bot := newLifecycleBot(t, router, func(request *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(request.URL.Path, "/sendMessage") {
					return testTelegramResponse(request), nil
				}
				if failure == "transport error" {
					return nil, errors.New("send failed")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: request,
					Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`))}, nil
			})
			update := callbackUpdate(buttonData(menu.Keyboard, "Custom timezone"))
			if err := bot.handleCallback(t.Context(), bot.bot.NewContext(update)); err == nil {
				t.Fatal("prompt send failure was not reported")
			}
			if actions, err := router.Handle(t.Context(), Update{UserID: 42, ChatType: PrivateChat, Text: "UTC"}); err != nil || len(actions) != 0 {
				t.Fatalf("failed prompt accepted a reply: %v, %v", actions, err)
			}
		})
	}
}
