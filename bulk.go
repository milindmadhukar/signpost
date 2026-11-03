package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// CSV import and export. The format is the same both ways:
//
//	source,target,status,note,tags
//	milind.dev/ade,https://example.com,302,ADE,"events,2026"
//	go.example.com/,https://example.com/long        <- blank path: a generated slug
//	events.example.com/*,https://luma.com/{path},301
//
// A header row is optional. With one, columns are matched by name (and
// unknown ones, like the export's clicks, are ignored); without one they are
// taken in the order above.

var csvAliases = map[string]string{
	"source": "source", "url": "source", "from": "source", "short": "source", "link": "source", "short_url": "source",
	"target": "target", "destination": "target", "to": "target", "goes_to": "target", "long_url": "target",
	"status": "status", "code": "status",
	"note": "note", "notes": "note", "title": "note",
	"tags": "tags", "tag": "tags",
}

var csvOrder = []string{"source", "target", "status", "note", "tags"}

func parseCSV(r io.Reader) ([]linkInput, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	cr.Comment = '#'
	recs, err := cr.ReadAll()
	if err != nil {
		return nil, fail(400, "CSV: "+err.Error())
	}
	cols := map[string]int{}
	if len(recs) > 0 {
		for i, h := range recs[0] {
			key := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))), " ", "_")
			if name, ok := csvAliases[key]; ok {
				cols[name] = i
			}
		}
	}
	if _, ok := cols["source"]; ok {
		recs = recs[1:]
	} else {
		cols = map[string]int{}
		for i, name := range csvOrder {
			cols[name] = i
		}
	}
	get := func(rec []string, name string) string {
		if i, ok := cols[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	out := []linkInput{}
	for _, rec := range recs {
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue
		}
		host, path := splitSource(get(rec, "source"))
		in := linkInput{Host: host, Path: path, Target: get(rec, "target"), Note: get(rec, "note")}
		if s := get(rec, "status"); s != "" {
			in.Status, _ = strconv.Atoi(s)
			if in.Status == 0 {
				in.Status = -1 // let validation report it, rather than silently use 302
			}
		}
		if t := get(rec, "tags"); t != "" {
			in.Tags = []string{t}
		}
		out = append(out, in)
	}
	if len(out) == 0 {
		return nil, fail(400, "no rows found")
	}
	return out, nil
}

// splitSource turns "https://milind.dev/ade" or "milind.dev/ade" into host
// and path. "milind.dev" and "milind.dev/" leave the path empty, which means
// "generate a slug".
func splitSource(s string) (string, string) {
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "https://"), "http://")
	host, path, ok := strings.Cut(s, "/")
	if !ok || path == "" {
		return host, ""
	}
	return host, "/" + path
}

func writeCSV(w io.Writer, links []Link) error {
	cw := csv.NewWriter(w)
	cw.Write([]string{"source", "target", "status", "note", "tags", "clicks", "created_at"})
	for _, l := range links {
		cw.Write([]string{
			l.URL(), l.Target, strconv.Itoa(l.Status), l.Note, joinTags(l.Tags),
			strconv.FormatInt(l.Clicks, 10), fmt.Sprint(l.CreatedAt),
		})
	}
	cw.Flush()
	return cw.Error()
}
