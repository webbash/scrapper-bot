package bazaraki

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

type snapshot struct {
	canonical string
	queries   []snapshotQuery
	textRows  map[string]string
}

type snapshotQuery struct {
	Key   json.RawMessage `json:"queryKey"`
	State struct {
		Data json.RawMessage `json:"data"`
	} `json:"state"`
}

func (q snapshotQuery) target(namespace, kind string) string {
	// The key can also be a React reference to another query, not an array.
	var key [4]string
	if json.Unmarshal(q.Key, &key) == nil && key[0] == namespace && key[1] == kind {
		return key[3]
	}
	return ""
}

func readSnapshot(r io.Reader) (snapshot, error) {
	var doc snapshot
	var wire []byte
	const push = "self.__next_f.push("
	z := html.NewTokenizer(r)
	for {
		switch z.Next() {
		case html.ErrorToken:
			if err := z.Err(); err != io.EOF {
				return snapshot{}, fmt.Errorf("read HTML: %w", err)
			}
			err := doc.readRows(wire)
			return doc, err
		case html.StartTagToken, html.SelfClosingTagToken:
			tag, hasAttr := z.TagName()
			switch string(tag) {
			case "link":
				var rel, href string
				for hasAttr {
					var key, value []byte
					key, value, hasAttr = z.TagAttr()
					switch string(key) {
					case "rel":
						rel = string(value)
					case "href":
						href = string(value)
					}
				}
				if doc.canonical == "" && strings.EqualFold(rel, "canonical") {
					doc.canonical = href
				}
			case "script":
				if z.Next() != html.TextToken {
					continue
				}
				js := bytes.TrimSpace(z.Text())
				js, ok := bytes.CutPrefix(js, []byte(push))
				if !ok {
					continue
				}
				js = bytes.TrimSpace(bytes.TrimSuffix(js, []byte(";")))
				js, ok = bytes.CutSuffix(js, []byte(")"))
				if !ok {
					continue
				}
				var packet []json.RawMessage
				if json.Unmarshal(js, &packet) != nil || len(packet) < 2 || !bytes.Equal(packet[0], []byte("1")) {
					continue
				}
				var chunk string
				if err := json.Unmarshal(packet[1], &chunk); err != nil {
					return snapshot{}, fmt.Errorf("decode inline HTML data: %w", err)
				}
				wire = append(wire, chunk...)
			}
		}
	}
}

// readRows reads the two forms observed in the browser snapshots: newline-
// terminated records and T rows whose length is in UTF-8 bytes. A T row does
// not necessarily end in a newline before the next record.
func (doc *snapshot) readRows(wire []byte) error {
	doc.textRows = make(map[string]string)
	for wire = bytes.TrimLeft(wire, "\r\n"); len(wire) != 0; wire = bytes.TrimLeft(wire, "\r\n") {
		if bytes.HasPrefix(wire, []byte(":H")) {
			// Resource hints have no record ID and never contain ad data.
			_, wire, _ = bytes.Cut(wire, []byte("\n"))
			continue
		}
		sep := bytes.IndexByte(wire, ':')
		if sep < 1 || sep > 16 || !isHex(string(wire[:sep])) {
			return fmt.Errorf("unrecognized inline HTML record header")
		}
		id := string(wire[:sep])
		wire = wire[sep+1:]
		if len(wire) == 0 {
			return fmt.Errorf("truncated inline HTML record")
		}
		if wire[0] == 'T' {
			comma := bytes.IndexByte(wire, ',')
			if comma < 2 || comma > 17 {
				return fmt.Errorf("unrecognized inline HTML text length")
			}
			n, err := strconv.ParseUint(string(wire[1:comma]), 16, 64)
			wire = wire[comma+1:]
			if err != nil || n > uint64(len(wire)) {
				return fmt.Errorf("invalid or truncated inline HTML text")
			}
			doc.textRows[id] = string(wire[:int(n)])
			wire = wire[int(n):]
			continue
		}

		line, rest, _ := bytes.Cut(wire, []byte("\n"))
		wire = rest
		if !bytes.HasPrefix(line, []byte(`["$","$L`)) || !bytes.Contains(line, []byte(`"queryKey"`)) {
			continue
		}
		var component []json.RawMessage
		if err := json.Unmarshal(line, &component); err != nil {
			return fmt.Errorf("decode inline HTML component: %w", err)
		}
		if len(component) < 4 {
			continue
		}
		var props struct {
			State struct {
				Queries []snapshotQuery `json:"queries"`
			} `json:"state"`
		}
		if err := json.Unmarshal(component[3], &props); err != nil {
			return fmt.Errorf("decode inline HTML queries: %w", err)
		}
		doc.queries = append(doc.queries, props.State.Queries...)
	}
	return nil
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func (doc snapshot) text(s string) string {
	if strings.HasPrefix(s, "$$") {
		return strings.TrimSpace(s[1:])
	}
	if id, ok := strings.CutPrefix(s, "$"); ok && (isHex(id) || id == "undefined") {
		return strings.TrimSpace(doc.textRows[id])
	}
	return strings.TrimSpace(s)
}
