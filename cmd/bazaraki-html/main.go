// bazaraki-html extracts JSON from individual live or saved Bazaraki pages.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/webbash/scrapper-bot/internal/bazaraki"
)

func main() {
	if err := runApp(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, output io.Writer, fetchHTML func(context.Context, string) ([]byte, error)) error {
	if len(args) != 2 {
		return fmt.Errorf("%s", usage)
	}
	enc := json.NewEncoder(output)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)

	switch args[0] {
	case "fetch", "fetch-html":
		body, err := fetchHTML(ctx, args[1])
		if err != nil {
			return err
		}
		if args[0] == "fetch-html" {
			_, err = output.Write(body)
			return err
		}
		page, err := bazaraki.ParsePage(bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("parse fetched Bazaraki page: %w", err)
		}
		return enc.Encode(page)
	case "search", "listing":
		// Parse a file or a pipeline without making any network requests.
	default:
		return fmt.Errorf("%s", usage)
	}

	input := stdin
	if args[1] != "-" {
		f, err := os.Open(args[1])
		if err != nil {
			return err
		}
		defer f.Close()
		input = f
	}

	if args[0] == "search" {
		page, err := bazaraki.ParseSearch(input)
		if err != nil {
			return err
		}
		return enc.Encode(page)
	}
	ad, err := bazaraki.ParseListing(input)
	if err != nil {
		return err
	}
	return enc.Encode(ad)
}
