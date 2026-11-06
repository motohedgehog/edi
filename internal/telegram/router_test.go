package telegram_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/config"
	"github.com/motohedgehog/edi/internal/logging/logtest"
	"github.com/motohedgehog/edi/internal/message"
	"github.com/motohedgehog/edi/internal/store"
	"github.com/motohedgehog/edi/internal/telegram"
	"github.com/motohedgehog/edi/internal/tfl"
)

type menuTestClock struct{ now time.Time }

func (c *menuTestClock) Now() time.Time { return c.now }

type statusCheckFunc func(context.Context, []string, string) ([]message.Chunk, error)

func (check statusCheckFunc) Check(ctx context.Context, lines []string, timezone string) ([]message.Chunk, error) {
	return check(ctx, lines, timezone)
}

func testCatalog(lines ...tfl.Line) func() []tfl.Line {
	return func() []tfl.Line { return append([]tfl.Line(nil), lines...) }
}

func TestRouterAccessRules(t *testing.T) {
	router := telegram.NewRouter(nil, telegram.RouterOptions{AllowedUsers: []int64{42}})
	actions, err := router.Handle(context.Background(), telegram.Update{UserID: 9, ChatType: telegram.PrivateChat, Command: "/start"})
	if err != nil || len(actions) != 1 || actions[0].Text != telegram.UnauthorizedText {
		t.Fatalf("unauthorized: %+v err=%v", actions, err)
	}
	actions, err = router.Handle(context.Background(), telegram.Update{UserID: 42, ChatType: telegram.GroupChat, Command: "/start"})
	if err != nil || len(actions) != 0 {
		t.Fatalf("group chat answered: %+v err=%v", actions, err)
	}
	actions, err = router.Handle(context.Background(), telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, Command: "/start"})
	if err != nil || len(actions) != 1 {
		t.Fatalf("start: %+v err=%v", actions, err)
	}
}

func TestRouterCommandsLeadToTfLControls(t *testing.T) {
	for _, test := range []struct {
		command string
		text    string
		button  string
	}{
		{"/start", "Available commands:", "Open menu"},
		{"/help", "Use /tfl", "Open menu"},
		{"/tfl", "<b>TfL overview</b>", "Choose lines"},
	} {
		t.Run(test.command, func(t *testing.T) {
			router := telegram.NewRouter(nil, telegram.RouterOptions{AllowedUsers: []int64{42}})
			actions, err := router.Handle(t.Context(), telegram.Update{
				UserID: 42, ChatType: telegram.PrivateChat, Command: test.command,
			})
			if err != nil || len(actions) != 1 {
				t.Fatalf("command: actions=%+v err=%v", actions, err)
			}
			if actions[0].Kind != telegram.ActionSend || !strings.Contains(actions[0].Text, test.text) {
				t.Fatalf("command response = %+v, want guidance or controls containing %q", actions[0], test.text)
			}
			data := findButton(actions[0].Keyboard, test.button)
			if test.command != telegram.CommandRoot {
				actions, err = router.Handle(t.Context(), telegram.Update{
					UserID: 42, ChatType: telegram.PrivateChat, CallbackData: data,
				})
				if err != nil || len(actions) != 1 || !strings.Contains(actions[0].Text, "<b>TfL overview</b>") {
					t.Fatalf("open controls from guidance: actions=%+v err=%v", actions, err)
				}
				data = findButton(actions[0].Keyboard, "Choose lines")
			}
			if data == "" {
				t.Fatal("TfL controls have no line selection callback")
			}
		})
	}
}

func TestRouterSavesProfileWithTelegramUserID(t *testing.T) {
	ctx := context.Background()
	t.Setenv("EDI_TELEGRAM_TOKEN", "test-token")
	t.Setenv("EDI_TELEGRAM_ALLOWED_CHATS", "42")
	t.Setenv("EDI_TFL_MONITOR_DEFAULT_TIMEZONE", " Europe/London ")
	cfg, err := config.Load(config.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "edi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()

	if _, found, err := database.GetProfile(ctx, 42, store.PersonaTFL); err != nil || found {
		t.Fatalf("profile already exists: found=%v err=%v", found, err)
	}
	router := telegram.NewRouter(database, telegram.RouterOptions{
		AllowedUsers: []int64{42},
		DefaultZone:  cfg.TfL.Monitor.DefaultTimezone,
		Catalog:      testCatalog(tfl.Line{ID: "central", DisplayName: "Central"}),
	})
	call := func(update telegram.Update) []telegram.Action {
		actions, err := router.Handle(ctx, update)
		if err != nil {
			t.Fatalf("handle %+v: %v", update, err)
		}
		if len(actions) != 1 {
			t.Fatalf("handle %+v: got %d actions, want 1", update, len(actions))
		}
		return actions
	}

	menu := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, Command: telegram.CommandRoot})[0]
	lines := findButton(menu.Keyboard, "Choose lines")
	linePicker := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: lines})[0]
	central := findButton(linePicker.Keyboard, "Central")
	linePicker = call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: central})[0]
	save := findButton(linePicker.Keyboard, "Save")
	call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: save})

	profile, found, err := database.GetProfile(ctx, 42, store.PersonaTFL)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("saved profile was not found")
	}
	if profile.UserID != 42 || profile.PersonaID != store.PersonaTFL || !profile.Enabled || profile.ScheduleMode != store.SchedulePeak || len(profile.Lines) != 1 || profile.Lines[0] != "central" {
		t.Fatalf("unexpected saved profile: %+v", profile)
	}
	if profile.Timezone != "Europe/London" {
		t.Fatalf("saved timezone = %q", profile.Timezone)
	}
}

func findButton(keyboard [][]telegram.Button, text string) string {
	for _, row := range keyboard {
		for _, button := range row {
			label := button.Text
			for _, prefix := range []string{"🚇", "🕒", "⏸", "▶️", "🔎", "⚙️"} {
				label = strings.TrimSpace(strings.TrimPrefix(label, prefix))
			}
			label = strings.TrimSpace(strings.TrimPrefix(label, "✓"))
			label = strings.TrimSuffix(label, " ✓")
			if button.Text == text || label == text {
				return button.Data
			}
		}
	}
	panic("button not found: " + text)
}

func TestRouterScreenDraftsSaveIndependently(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "edi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	router := telegram.NewRouter(database, telegram.RouterOptions{
		AllowedUsers: []int64{42},
		Catalog:      testCatalog(tfl.Line{ID: "central", DisplayName: "Central"}),
	})
	call := func(update telegram.Update) telegram.Action {
		actions, callErr := router.Handle(ctx, update)
		if callErr != nil {
			t.Fatal(callErr)
		}
		if len(actions) != 1 {
			t.Fatalf("got %d actions, want one", len(actions))
		}
		return actions[0]
	}
	menu := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, Command: telegram.CommandRoot})
	lines := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(menu.Keyboard, "Choose lines")})
	lines = call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(lines.Keyboard, "Central")})
	call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(lines.Keyboard, "Discard")})
	if _, found, err := database.GetProfile(ctx, 42, store.PersonaTFL); err != nil || found {
		t.Fatalf("discard created profile: found=%v err=%v", found, err)
	}

	menu = call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, Command: telegram.CommandRoot})
	lines = call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(menu.Keyboard, "Choose lines")})
	lines = call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(lines.Keyboard, "Central")})
	call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(lines.Keyboard, "Save")})

	profile, found, err := database.GetProfile(ctx, 42, store.PersonaTFL)
	if err != nil || !found || len(profile.Lines) != 1 || profile.Lines[0] != "central" {
		t.Fatalf("saved lines: %+v found=%v err=%v", profile, found, err)
	}
	menu = call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, Command: telegram.CommandRoot})
	hours := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(menu.Keyboard, "Alert hours")})
	hours = call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(hours.Keyboard, "Custom hours")})
	call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(hours.Keyboard, "Discard")})
	profile, _, err = database.GetProfile(ctx, 42, store.PersonaTFL)
	if err != nil || profile.ScheduleMode != store.SchedulePeak {
		t.Fatalf("discard rewrote schedule: %+v err=%v", profile, err)
	}

	menu = call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, Command: telegram.CommandRoot})
	lines = call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(menu.Keyboard, "Choose lines")})
	lines = call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(lines.Keyboard, "Central")})
	call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(lines.Keyboard, "Save")})
	profile, _, err = database.GetProfile(ctx, 42, store.PersonaTFL)
	if err != nil || len(profile.Lines) != 0 || !profile.Enabled {
		t.Fatalf("empty selection was not preserved: %+v err=%v", profile, err)
	}
}

func TestRouterStatusActionIsIdempotentAndPreservedByLaterSave(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "edi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	profile, err := database.SaveProfile(ctx, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, ScheduleMode: store.SchedulePeak,
		WindowStart: store.DefaultPeakStart, WindowEnd: store.DefaultPeakEnd, Timezone: "Europe/London",
		Lines: []string{"central"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	router := telegram.NewRouter(database, telegram.RouterOptions{AllowedUsers: []int64{42}, Catalog: testCatalog(tfl.Line{ID: "central", DisplayName: "Central"})})
	call := func(update telegram.Update) telegram.Action {
		actions, callErr := router.Handle(ctx, update)
		if callErr != nil || len(actions) != 1 {
			t.Fatalf("handle: actions=%+v err=%v", actions, callErr)
		}
		return actions[0]
	}
	menu := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, Command: telegram.CommandRoot})
	call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(menu.Keyboard, "Choose lines")})
	pause := findButton(menu.Keyboard, "Pause alerts")
	menu = call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: pause})
	call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: pause})
	call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(menu.Keyboard, "Save")})
	profile, _, err = database.GetProfile(ctx, 42, store.PersonaTFL)
	if err != nil || profile.Enabled {
		t.Fatalf("pause was reversed or lost: %+v err=%v", profile, err)
	}
}

func TestRouterRendersScannableMenusAndPlainSelectionButtons(t *testing.T) {
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "edi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	router := telegram.NewRouter(database, telegram.RouterOptions{
		AllowedUsers: []int64{42},
		Catalog: testCatalog(
			tfl.Line{ID: "central", DisplayName: "Central & <Line>"},
			tfl.Line{ID: "elizabeth", DisplayName: "Elizabeth line"},
		),
	})
	call := func(update telegram.Update) []telegram.Action {
		actions, err := router.Handle(context.Background(), update)
		if err != nil {
			t.Fatalf("handle: %v", err)
		}
		return actions
	}
	menuActions := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, Command: telegram.CommandRoot})
	if len(menuActions) != 1 || menuActions[0].Format != message.FormatHTML {
		t.Fatalf("overview action = %+v", menuActions)
	}
	menu := menuActions[0]
	for _, want := range []string{"<b>TfL overview</b>", "Saved lines (0)", "Alert status: enabled"} {
		if !strings.Contains(menu.Text, want) {
			t.Errorf("overview %q does not contain %q", menu.Text, want)
		}
	}
	for _, want := range []string{"🚇 Choose lines", "🕒 Alert hours", "🔎 Check status now", "⚙️ Configuration"} {
		if !hasButtonText(menu.Keyboard, want) {
			t.Errorf("overview keyboard is missing %q: %+v", want, menu.Keyboard)
		}
	}

	lines := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(menu.Keyboard, "Choose lines")})[0]
	if !strings.Contains(lines.Text, "<b>Choose lines</b>") || !strings.Contains(lines.Text, "Selected: 0") {
		t.Fatalf("line selection screen is missing its heading or count: %q", lines.Text)
	}
	centralButton := findButtonLabel(lines.Keyboard, "Central & <Line>")
	if centralButton != "Central & <Line>" {
		t.Fatalf("unselected line button = %q, want plain Central label", centralButton)
	}
	selected := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(lines.Keyboard, "Central & <Line>")})[0]
	for _, want := range []string{"Selected: 1", "Tap a line to select or deselect it.", "⚠️ Changes need saving."} {
		if !strings.Contains(selected.Text, want) {
			t.Errorf("selected-lines screen %q does not contain %q", selected.Text, want)
		}
	}
	selectedButton := findButtonLabel(selected.Keyboard, "Central & <Line>")
	if selectedButton != "Central & <Line> ✓" {
		t.Fatalf("selected line button = %q, want plain Central ✓ label", selectedButton)
	}
	multiple := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(selected.Keyboard, "Elizabeth line")})[0]
	removed := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(multiple.Keyboard, "Elizabeth line")})[0]
	empty := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(removed.Keyboard, "Central & <Line>")})[0]
	for i, action := range []telegram.Action{lines, selected, multiple, removed, empty} {
		wantCount := []int{0, 1, 2, 1, 0}[i]
		wantText := fmt.Sprintf("<b>Choose lines</b>\nSelected: %d\nAlert status: enabled\n\nTap a line to select or deselect it.", wantCount)
		if wantCount == 0 {
			wantText += "\nNo line alerts will be sent."
		} else {
			wantText += "\n⚠️ Changes need saving."
		}
		if action.Text != wantText {
			t.Errorf("selection step %d: text = %q, want %q", i, action.Text, wantText)
		}
		if hasButtonText(action.Keyboard, "Central & <Line> ✓") != (wantCount > 0) ||
			hasButtonText(action.Keyboard, "Elizabeth line ✓") != (wantCount == 2) {
			t.Errorf("selection step %d: incorrect checkmarks: %+v", i, action.Keyboard)
		}
	}
	selected = call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(empty.Keyboard, "Central & <Line>")})[0]
	saved := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(selected.Keyboard, "Save")})[0]
	if !strings.Contains(saved.Text, "Saved lines (1): <b>Central &amp; &lt;Line&gt;</b>") {
		t.Fatalf("overview lost escaped saved line name: %q", saved.Text)
	}
}

func TestRouterShowsUnavailableSavedLinesAndAlertState(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "edi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.SaveProfile(ctx, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: false, AllDay: true,
		Timezone: "Europe/London", Lines: []string{"district"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	router := telegram.NewRouter(database, telegram.RouterOptions{AllowedUsers: []int64{42}, Catalog: testCatalog(tfl.Line{ID: "central", DisplayName: "Central"})})
	actions, err := router.Handle(ctx, telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, Command: telegram.CommandRoot})
	if err != nil || len(actions) != 1 {
		t.Fatalf("open menu: actions=%+v err=%v", actions, err)
	}
	for _, want := range []string{"Alert status: paused", "⚠️ Unavailable saved lines: <b>district</b>"} {
		if !strings.Contains(actions[0].Text, want) {
			t.Errorf("overview %q does not contain %q", actions[0].Text, want)
		}
	}
	if !hasButtonText(actions[0].Keyboard, "▶️ Enable alerts") {
		t.Fatalf("paused menu has no enable action: %+v", actions[0].Keyboard)
	}
}

func TestRouterKeepsDraftCatalogStableAndRefreshesAfterExpiry(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "edi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.SaveProfile(ctx, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, AllDay: true,
		Timezone: "Europe/London", Lines: []string{"district"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	catalog := []tfl.Line{{ID: "district", DisplayName: "District Old"}}
	clock := &menuTestClock{now: time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)}
	router := telegram.NewRouter(database, telegram.RouterOptions{
		AllowedUsers: []int64{42},
		Catalog: func() []tfl.Line {
			return append([]tfl.Line(nil), catalog...)
		},
		Clock: clock,
	})
	open := func() telegram.Action {
		actions, err := router.Handle(ctx, telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, Command: telegram.CommandRoot})
		if err != nil || len(actions) != 1 {
			t.Fatalf("open menu: actions=%+v err=%v", actions, err)
		}
		return actions[0]
	}
	first := open()
	if !strings.Contains(first.Text, "District Old") {
		t.Fatalf("initial draft omitted its catalog name: %q", first.Text)
	}
	catalog = []tfl.Line{{ID: "central", DisplayName: "Central Fresh"}}
	stillOpen := open()
	if !strings.Contains(stillOpen.Text, "District Old") || strings.Contains(stillOpen.Text, "Central Fresh") {
		t.Fatalf("active draft changed catalog: %q", stillOpen.Text)
	}

	clock.now = clock.now.Add(telegram.DraftLifetime + time.Second)
	fresh := open()
	if !strings.Contains(fresh.Text, "district") || strings.Contains(fresh.Text, "District Old") {
		t.Fatalf("expired draft did not use fresh catalog and preserve saved ID: %q", fresh.Text)
	}
	lines, err := router.Handle(ctx, telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(fresh.Keyboard, "Choose lines")})
	if err != nil || len(lines) != 1 {
		t.Fatalf("open fresh line selector: actions=%+v err=%v", lines, err)
	}
	if !hasButtonText(lines[0].Keyboard, "Central Fresh") || !hasButtonText(lines[0].Keyboard, "district ✓") {
		t.Fatalf("fresh catalog or unavailable saved line missing from keyboard: %+v", lines[0].Keyboard)
	}
}

func TestRouterReturnsManualCheckChunksAndAvailabilityWarning(t *testing.T) {
	ctx := context.Background()
	chunks := []message.Chunk{
		{Body: "<b>Central</b>\n✅ <b>Good Service</b>", Format: message.FormatHTML},
		{Body: "Part 2/2\n<b>District</b>\n⚠️ <b>Minor Delays</b>", Format: message.FormatHTML},
	}
	router := telegram.NewRouter(nil, telegram.RouterOptions{
		AllowedUsers: []int64{42},
		Catalog:      testCatalog(tfl.Line{ID: "central", DisplayName: "Central"}),
		StatusChecker: statusCheckFunc(func(context.Context, []string, string) ([]message.Chunk, error) {
			return chunks, nil
		}),
	})
	menu := callRouter(ctx, t, router, telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, Command: telegram.CommandRoot})[0]
	lines := callRouter(ctx, t, router, telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(menu.Keyboard, "Choose lines")})[0]
	lines = callRouter(ctx, t, router, telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(lines.Keyboard, "Central")})[0]
	actions := callRouter(ctx, t, router, telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(lines.Keyboard, "Check selected lines")})
	if len(actions) != 2 {
		t.Fatalf("manual check returned %d actions, want 2 chunks", len(actions))
	}
	for i, action := range actions {
		if action.Kind != telegram.ActionSeparate || action.Format != message.FormatHTML || action.Text != chunks[i].Body {
			t.Errorf("chunk action %d = %+v", i+1, action)
		}
		if i == 0 && len(action.Keyboard) != 0 {
			t.Errorf("first chunk unexpectedly has navigation buttons: %+v", action.Keyboard)
		}
		if i == 1 && len(action.Keyboard) == 0 {
			t.Error("last chunk is missing navigation buttons")
		}
	}

	failedRouter := telegram.NewRouter(nil, telegram.RouterOptions{
		AllowedUsers: []int64{42},
		Catalog:      testCatalog(tfl.Line{ID: "central", DisplayName: "Central"}),
		StatusChecker: statusCheckFunc(func(context.Context, []string, string) ([]message.Chunk, error) {
			return nil, errors.New("private transport details")
		}),
	})
	menu = callRouter(ctx, t, failedRouter, telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, Command: telegram.CommandRoot})[0]
	lines = callRouter(ctx, t, failedRouter, telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(menu.Keyboard, "Choose lines")})[0]
	lines = callRouter(ctx, t, failedRouter, telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(lines.Keyboard, "Central")})[0]
	actions = callRouter(ctx, t, failedRouter, telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(lines.Keyboard, "Check selected lines")})
	if len(actions) != 1 || actions[0].Format != message.FormatHTML || !strings.Contains(actions[0].Text, "TfL updates unavailable") || strings.Contains(actions[0].Text, "private transport details") {
		t.Fatalf("manual availability warning = %+v", actions)
	}
}

func callRouter(ctx context.Context, t *testing.T, router *telegram.Router, update telegram.Update) []telegram.Action {
	t.Helper()
	actions, err := router.Handle(ctx, update)
	if err != nil {
		t.Fatalf("handle %+v: %v", update, err)
	}
	return actions
}

func findButtonLabel(keyboard [][]telegram.Button, want string) string {
	for _, row := range keyboard {
		for _, button := range row {
			if button.Text == want || button.Text == want+" ✓" {
				return button.Text
			}
		}
	}
	return ""
}

func hasButtonText(keyboard [][]telegram.Button, want string) bool {
	for _, row := range keyboard {
		for _, button := range row {
			if button.Text == want {
				return true
			}
		}
	}
	return false
}

func TestRouterInvalidTimeKeepsDraftAndRepeatsPrompt(t *testing.T) {
	ctx := context.Background()
	clock := &menuTestClock{now: time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)}
	router := telegram.NewRouter(nil, telegram.RouterOptions{AllowedUsers: []int64{42}, Clock: clock})
	call := func(update telegram.Update) telegram.Action {
		actions, err := router.Handle(ctx, update)
		if err != nil || len(actions) != 1 {
			t.Fatalf("handle: actions=%+v err=%v", actions, err)
		}
		return actions[0]
	}
	menu := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, Command: telegram.CommandRoot})
	hours := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(menu.Keyboard, "Alert hours")})
	hours = call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, CallbackData: findButton(hours.Keyboard, "Custom hours")})
	prompt := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, MessageID: 100, CallbackData: findButton(hours.Keyboard, "Start: 06:30")})
	if prompt.Kind != telegram.ActionForceReply || !strings.Contains(prompt.Text, "Current start") {
		t.Fatalf("unexpected prompt: %+v", prompt)
	}
	router.CompletePromptSend(42, prompt.PromptID, 500)
	invalid := call(telegram.Update{UserID: 42, ChatType: telegram.PrivateChat, Text: "bad", ReplyToMessageID: 500})
	if invalid.Kind != telegram.ActionForceReply || !strings.Contains(invalid.Text, "invalid") {
		t.Fatalf("invalid reply did not repeat prompt: %+v", invalid)
	}
}

func TestRouterOvergroundSharesLineDraft(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "edi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	initial, err := database.SaveProfile(ctx, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, AllDay: true,
		Timezone: "Europe/London", Lines: []string{"central", "removed"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	clock := &menuTestClock{now: time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)}
	var checked []string
	router := telegram.NewRouter(database, telegram.RouterOptions{
		AllowedUsers: []int64{42}, Clock: clock,
		Catalog: testCatalog(
			tfl.Line{ID: "central", DisplayName: "Central", ModeName: "tube"},
			tfl.Line{ID: "dlr", DisplayName: "DLR", ModeName: "dlr"},
			tfl.Line{ID: "elizabeth", DisplayName: "Elizabeth line", ModeName: "elizabeth-line"},
			tfl.Line{ID: "lioness", DisplayName: "Lioness", ModeName: "overground"},
			tfl.Line{ID: "windrush", DisplayName: "Windrush", ModeName: "overground"},
		),
		StatusChecker: statusCheckFunc(func(_ context.Context, lines []string, _ string) ([]message.Chunk, error) {
			checked = append([]string(nil), lines...)
			return []message.Chunk{{Body: "status"}}, nil
		}),
	})
	call := func(update telegram.Update) telegram.Action {
		update.UserID, update.ChatType = 42, telegram.PrivateChat
		actions := callRouter(ctx, t, router, update)
		if len(actions) != 1 {
			t.Fatalf("got %d actions", len(actions))
		}
		return actions[0]
	}
	click := func(action telegram.Action, label string) telegram.Action {
		return call(telegram.Update{CallbackData: findButton(action.Keyboard, label)})
	}
	menu := call(telegram.Update{Command: telegram.CommandRoot})
	main := click(menu, "Choose lines")
	if hasButtonText(main.Keyboard, "Lioness") || hasButtonText(main.Keyboard, "Windrush") {
		t.Fatal("Overground lines leaked into main list")
	}
	for _, label := range []string{"Central ✓", "DLR", "Elizabeth line", "removed ✓", "Overground"} {
		if !hasButtonText(main.Keyboard, label) {
			t.Fatalf("main list missing %s", label)
		}
	}
	unchanged, _, err := database.GetProfile(ctx, 42, store.PersonaTFL)
	if err != nil || !slices.Equal(unchanged.Lines, initial.Lines) || unchanged.Revision != initial.Revision {
		t.Fatalf("opening menu changed profile: %+v err=%v", unchanged, err)
	}
	main = click(main, "removed")
	main = click(main, "DLR")
	main = click(main, "Elizabeth line")
	overground := click(main, "Overground")
	if !strings.Contains(overground.Text, "Choose Overground lines") || hasButtonText(overground.Keyboard, "Central ✓") {
		t.Fatalf("wrong submenu: %+v", overground)
	}
	for _, label := range []string{"Lioness", "Windrush", "Back", "Save", "Discard", "🔎 Check selected lines"} {
		if !hasButtonText(overground.Keyboard, label) {
			t.Fatalf("submenu missing %s", label)
		}
	}
	overground = click(overground, "Lioness")
	if !strings.Contains(overground.Text, "Selected: 4\n") ||
		!strings.Contains(overground.Text, "Tap a line to select or deselect it.") ||
		strings.Contains(overground.Text, "Selected lines:") ||
		!hasButtonText(overground.Keyboard, "Lioness ✓") {
		t.Fatalf("unstable Overground selection summary: %+v", overground)
	}
	want := []string{"central", "dlr", "elizabeth", "lioness"}
	check := click(overground, "Check selected lines")
	if !slices.Equal(checked, want) || !hasButtonText(check.Keyboard, "Lioness ✓") {
		t.Fatalf("submenu check lost complete draft: %v", checked)
	}
	main = click(check, "Back")
	if !hasButtonText(main.Keyboard, "DLR ✓") || !hasButtonText(main.Keyboard, "Elizabeth line ✓") {
		t.Fatal("Back lost selections")
	}
	clock.now = clock.now.Add(telegram.ManualRateLimit)
	click(main, "Check selected lines")
	if !slices.Equal(checked, want) {
		t.Fatalf("main check lost Overground: %v", checked)
	}
	overground = click(main, "Overground")
	menu = click(overground, "Save")
	if !strings.Contains(menu.Text, "TfL overview") {
		t.Fatalf("save did not return to overview: %s", menu.Text)
	}
	saved, _, err := database.GetProfile(ctx, 42, store.PersonaTFL)
	if err != nil || !slices.Equal(saved.Lines, want) {
		t.Fatalf("save lost selections: %+v err=%v", saved, err)
	}
	main = click(menu, "Choose lines")
	main = click(main, "Central")
	overground = click(main, "Overground")
	overground = click(overground, "Windrush")
	menu = click(overground, "Discard")
	if !strings.Contains(menu.Text, "TfL overview") {
		t.Fatal("discard did not return to overview")
	}
	main = click(menu, "Choose lines")
	overground = click(main, "Overground")
	if !hasButtonText(main.Keyboard, "Central ✓") || hasButtonText(overground.Keyboard, "Windrush ✓") {
		t.Fatal("discard did not restore whole selection")
	}
	clock.now = clock.now.Add(telegram.DraftLifetime + time.Second)
	expired := click(overground, "Lioness")
	if !strings.Contains(expired.Text, "expired") {
		t.Fatal("submenu callback bypassed expiry")
	}
}

func TestRouterLogsUserIDsOnlyAtDebug(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			logger, logs := logtest.New(level)
			router := telegram.NewRouter(nil, telegram.RouterOptions{Logger: logger, AllowedUsers: []int64{123456789}})
			for _, id := range []int64{123456789, 987654321} {
				if _, err := router.Handle(t.Context(), telegram.Update{UserID: id, ChatType: telegram.PrivateChat, Command: "/help"}); err != nil {
					t.Fatal(err)
				}
			}
			logs.AssertPrivate(t)
			if level == slog.LevelInfo {
				logs.AssertPrivate(t, "123456789", "987654321")
			}
		})
	}
}
