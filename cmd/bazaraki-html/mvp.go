package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/webbash/scrapper-bot/internal/bazaraki"
	"github.com/webbash/scrapper-bot/internal/housing"
	"github.com/webbash/scrapper-bot/internal/telegram"
)

const usage = "usage: bazaraki-html <fetch|fetch-html> <url> | <search|listing> <file|-> | chat-id | bot | sync [search-url] | test-card <file>"

func runApp(args []string, input io.Reader, output, logs io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "sync", "bot", "chat-id", "test-card":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return runMVP(ctx, args, output, logs)
	default:
		return run(context.Background(), args, input, output, (bazaraki.Fetcher{}).FetchHTML)
	}
}

func runMVP(ctx context.Context, args []string, output, logs io.Writer) error {
	mode := args[0]
	switch mode {
	case "sync":
		if len(args) > 2 {
			return errors.New(usage)
		}
	case "test-card":
		if len(args) != 2 {
			return errors.New(usage)
		}
	default:
		if len(args) != 1 {
			return errors.New(usage)
		}
	}
	var searchURL string
	if mode == "sync" {
		searchURL = strings.TrimSpace(os.Getenv("BAZARAKI_SEARCH_URL"))
		if len(args) == 2 {
			searchURL = args[1]
		}
		if searchURL == "" {
			return errors.New("pass the search URL to sync or set BAZARAKI_SEARCH_URL")
		}
	}
	api, err := telegram.New(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if err != nil {
		return err
	}
	if mode == "chat-id" {
		fmt.Fprintln(logs, "Send /chatid@YourBotUsername in your Telegram group (Ctrl+C to stop).")
		return printChatID(ctx, api, output)
	}
	chatID, err := strconv.ParseInt(os.Getenv("TELEGRAM_CHAT_ID"), 10, 64)
	if err != nil || chatID == 0 {
		return errors.New("set TELEGRAM_CHAT_ID to the ID of your private group (see chat-id)")
	}
	filename := os.Getenv("HOUSING_DB_PATH")
	if filename == "" {
		filename = "housing.sqlite"
	}
	store, err := housing.Open(ctx, filename)
	if err != nil {
		return err
	}
	defer store.Close()
	bot := housing.Bot{ChatID: chatID, Store: store, API: api}
	if mode == "bot" {
		fmt.Fprintln(logs, "Telegram bot is listening. Bazaraki is only checked by manual sync.")
		return bot.Run(ctx, logs)
	}
	if mode == "test-card" {
		f, err := os.Open(args[1])
		if err != nil {
			return err
		}
		defer f.Close()
		ad, err := bazaraki.ParseListing(f)
		if err != nil {
			return err
		}
		var received telegram.Message
		sent, err := housing.SendTestCard(ctx, store, chatID, ad, func(ctx context.Context, chat int64, card bazaraki.Listing) (int64, error) {
			msg, err := bot.DeliverListing(ctx, chat, card)
			received = msg
			return msg.ID, err
		})
		if err != nil {
			return err
		}
		if sent {
			buttons := 0
			if received.Keyboard != nil {
				for _, row := range received.Keyboard.Rows {
					buttons += len(row)
				}
			}
			p := ad.Location.Coordinates
			coords := fmt.Sprintf("%.6f,%.6f", p.Latitude, p.Longitude)
			withCoordinates := strings.Contains(received.Text+received.Caption, coords)
			fmt.Fprintf(output, "Telegram подтвердил карточку: фото=%t, координаты=%t, кнопок=%d.\n", len(received.Photo) != 0, withCoordinates, buttons)
		} else {
			fmt.Fprintln(output, "Тестовая карточка уже отправлена или результат предыдущей попытки неизвестен; повторной отправки нет.")
		}
		return nil
	}
	report, syncErr := housing.Sync(ctx, store, chatID, searchURL, (bazaraki.Fetcher{}).FetchHTML, bot.SendListing)
	if report.Baseline {
		fmt.Fprintf(output, "Стартовая выборка: %d объявлений сохранено без рассылки.\n", report.New)
	} else {
		fmt.Fprintf(output, "Выдача: %d; новых ID: %d; отправлено: %d.\n", report.Results, report.New, report.Sent)
	}
	if report.Unavailable > 0 {
		fmt.Fprintf(logs, "Пропущено недоступных карточек: %d.\n", report.Unavailable)
	}
	if ctx.Err() == nil {
		held, err := store.HeldCount(ctx, chatID)
		if err != nil {
			return errors.Join(syncErr, err)
		}
		if held > 0 {
			fmt.Fprintf(logs, "Неподтверждённых попыток отправки: %d. Автоповтора нет, чтобы не прислать дубли.\n", held)
		}
	}
	return syncErr
}

func printChatID(ctx context.Context, api *telegram.Client, output io.Writer) error {
	var offset int64
	for ctx.Err() == nil {
		updates, err := api.Updates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, u := range updates {
			if u.ID < offset {
				continue
			}
			offset = u.ID + 1
			if u.Message != nil && u.Message.Chat.ID != 0 && housing.IsChatIDCommand(u.Message.Text) {
				_, err := fmt.Fprintf(output, "%d\n", u.Message.Chat.ID)
				return err
			}
		}
	}
	return nil
}
