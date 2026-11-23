package housing

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/webbash/scrapper-bot/internal/bazaraki"
	"github.com/webbash/scrapper-bot/internal/telegram"
)

type Telegram interface {
	SendPhoto(context.Context, int64, string, string, *telegram.Keyboard) (telegram.Message, error)
	SendText(context.Context, int64, string, string, *telegram.Keyboard) (telegram.Message, error)
	Answer(context.Context, string, string) error
	DeleteMessage(context.Context, int64, int64) error
	CollapseMessage(context.Context, int64, int64, bool) error
	Updates(context.Context, int64) ([]telegram.Update, error)
}

type Bot struct {
	ChatID int64
	Store  *Store
	API    Telegram
}

func (b Bot) SendListing(ctx context.Context, chatID int64, ad bazaraki.Listing) (int64, error) {
	msg, err := b.DeliverListing(ctx, chatID, ad)
	return msg.ID, err
}

func (b Bot) DeliverListing(ctx context.Context, chatID int64, ad bazaraki.Listing) (telegram.Message, error) {
	if chatID != b.ChatID {
		return telegram.Message{}, errors.New("cannot send housing alerts to an unconfigured chat")
	}
	id := strconv.FormatInt(ad.ID, 10)
	keyboard := &telegram.Keyboard{Rows: [][]telegram.Button{
		{{Text: "Подходит", Callback: "yes:" + id}, {Text: "Не подходит", Callback: "no:" + id}},
		{{Text: "Объявление", URL: ad.URL}, {Text: "Фавориты", Callback: "favorites:0"}},
	}}

	var lines []string
	lines = append(lines, shorten(ad.Title, 180))
	if ad.PriceDisplay != "" {
		lines = append(lines, shorten(ad.PriceDisplay, 60))
	}
	if ad.Location.Name != "" {
		lines = append(lines, shorten(ad.Location.Name, 180))
	}
	if ad.ConstructionYear != nil {
		lines = append(lines, fmt.Sprintf("Год постройки: %d", *ad.ConstructionYear))
	}
	if p := ad.Location.Coordinates; p != nil {
		label := "Точка объявления"
		if p.Approximate == nil {
			label = "Точность точки не указана"
		} else if *p.Approximate {
			label = "Приблизительная точка"
		}
		coords := fmt.Sprintf("%.6f,%.6f", p.Latitude, p.Longitude)
		lines = append(lines, label+": "+coords)
		locationURL := "https://www.google.com/maps/search/?api=1&query=" + url.QueryEscape(coords)
		keyboard.Rows = append(keyboard.Rows, []telegram.Button{{Text: "На карте", URL: locationURL}})
	}
	text := strings.Join(lines, "\n")
	if ad.PhotoURL != "" {
		msg, err := b.API.SendPhoto(ctx, chatID, ad.PhotoURL, text, keyboard)
		if err == nil {
			return msg, nil
		}
		var apiErr *telegram.APIError
		// Fall back only when Telegram explicitly rejected the photo.
		// An ambiguous timeout may mean it already posted the photo.
		if !errors.As(err, &apiErr) || apiErr.Code != 400 {
			return telegram.Message{}, err
		}
	}
	return b.API.SendText(ctx, chatID, text, "", keyboard)
}

func (b Bot) Run(ctx context.Context, logs io.Writer) error {
	var offset int64
	for ctx.Err() == nil {
		updates, err := b.API.Updates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var apiErr *telegram.APIError
			if errors.As(err, &apiErr) && apiErr.Rejected() && apiErr.Code != 429 {
				return err
			}
			fmt.Fprintln(logs, err)
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
				continue
			}
		}
		for _, update := range updates {
			if update.ID < offset {
				continue
			}
			if err := b.Handle(ctx, update); err != nil && ctx.Err() == nil {
				fmt.Fprintln(logs, err)
			}
			offset = update.ID + 1
		}
	}
	return nil
}

func (b Bot) Handle(ctx context.Context, u telegram.Update) error {
	if m := u.Message; m != nil && m.Chat.ID == b.ChatID {
		switch command(m.Text) {
		case "/favorites":
			return b.showFavorites(ctx, 0)
		case "/start", "/help":
			_, err := b.API.SendText(ctx, b.ChatID, "Здесь будут новые квартиры. Нажмите «Подходит», чтобы добавить объявление в Фавориты.", "", &telegram.Keyboard{
				Rows: [][]telegram.Button{{{Text: "Фавориты", Callback: "favorites:0"}}},
			})
			return err
		}
	}
	q := u.Callback
	if q == nil || q.Message == nil || q.Message.Chat.ID != b.ChatID {
		return nil
	}
	action, raw, ok := strings.Cut(q.Data, ":")
	value, err := strconv.ParseInt(raw, 10, 64)
	if !ok || err != nil {
		return b.API.Answer(ctx, q.ID, "Не удалось распознать кнопку")
	}
	switch action {
	case "yes", "no":
		if value == 0 {
			return b.API.Answer(ctx, q.ID, "Не удалось распознать объявление")
		}
		decision, reply := Suitable, "Добавлено в Фавориты"
		if action == "no" {
			decision, reply = Unsuitable, "Отмечено как «Не подходит»"
		}
		updated, err := b.Store.Decide(ctx, b.ChatID, value, q.Message.ID, decision)
		if err != nil {
			_ = b.API.Answer(ctx, q.ID, "Не удалось сохранить. Попробуйте ещё раз.")
			return err
		}
		if !updated {
			return b.API.Answer(ctx, q.ID, "Карточка не найдена. Если она только что пришла, попробуйте ещё раз.")
		}
		answered := b.API.Answer(ctx, q.ID, reply)
		if decision != Unsuitable {
			return answered
		}
		return errors.Join(answered, b.hideRejected(ctx, q.Message))
	case "favorites":
		if value < 0 || value > 1_000_000 {
			return b.API.Answer(ctx, q.ID, "Страница не найдена")
		}
		return errors.Join(b.API.Answer(ctx, q.ID, ""), b.showFavorites(ctx, int(value)))
	default:
		return b.API.Answer(ctx, q.ID, "Неизвестная кнопка")
	}
}

func (b Bot) hideRejected(ctx context.Context, message *telegram.Message) error {
	err := b.API.DeleteMessage(ctx, b.ChatID, message.ID)
	if err == nil {
		return nil
	}
	var apiErr *telegram.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Code == 400 && strings.Contains(strings.ToLower(apiErr.Description), "message to delete not found") {
			return nil
		}
		if apiErr.Code == 400 || apiErr.Code == 403 {
			// For example, the Telegram deletion window may have expired.
			if editErr := b.API.CollapseMessage(ctx, b.ChatID, message.ID, len(message.Photo) > 0); editErr == nil {
				return nil
			} else {
				return errors.Join(err, editErr)
			}
		}
	}
	return err
}

func (b Bot) showFavorites(ctx context.Context, offset int) error {
	const pageSize = 8
	ads, err := b.Store.Favorites(ctx, b.ChatID, offset, pageSize+1)
	if err != nil {
		return err
	}
	hasNext := len(ads) > pageSize
	if hasNext {
		ads = ads[:pageSize]
	}
	var text strings.Builder
	text.WriteString("<b>Фавориты</b>")
	for _, ad := range ads {
		fmt.Fprintf(&text, "\n• <a href=\"%s\">%s</a>", html.EscapeString(ad.URL), html.EscapeString(shorten(ad.Title, 125)))
		if ad.PriceDisplay != "" || ad.Location.Name != "" {
			fmt.Fprintf(&text, " — %s", html.EscapeString(shorten(strings.TrimSpace(ad.PriceDisplay+" "+ad.Location.Name), 140)))
		}
	}
	if len(ads) == 0 {
		text.WriteString("\nЗдесь пока нет объявлений.")
	}
	var buttons []telegram.Button
	if offset >= pageSize {
		buttons = append(buttons, telegram.Button{Text: "Назад", Callback: fmt.Sprintf("favorites:%d", offset-pageSize)})
	}
	if hasNext {
		buttons = append(buttons, telegram.Button{Text: "Далее", Callback: fmt.Sprintf("favorites:%d", offset+pageSize)})
	}
	var keyboard *telegram.Keyboard
	if len(buttons) != 0 {
		keyboard = &telegram.Keyboard{Rows: [][]telegram.Button{buttons}}
	}
	_, err = b.API.SendText(ctx, b.ChatID, text.String(), "HTML", keyboard)
	return err
}

func IsChatIDCommand(text string) bool { return command(text) == "/chatid" }

func command(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	name, _, _ := strings.Cut(fields[0], "@")
	return name
}

func shorten(text string, max int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > max {
		return string(runes[:max-1]) + "…"
	}
	return string(runes)
}
