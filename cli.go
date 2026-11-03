package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

type client struct{ base, token string }

var takesValue = map[string]bool{"note": true, "resolver": true, "fallback": true, "tags": true, "password": true}

func (c client) do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// splitURL turns "milind.dev/helloworld" (or https://milind.dev/x) into host
// and path. Only an explicit /* means the whole host; a bare host or a
// trailing slash stays "/", which the server rejects, so a typo can never take
// over a whole site.
func splitURL(s string) (string, string) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	host, path, _ := strings.Cut(s, "/")
	path = "/" + path
	if path == "/*" {
		path = "*"
	}
	return strings.ToLower(host), path
}

// resolve accepts a numeric id or host/path.
func (c client) resolve(ref string) (Link, error) {
	var links []Link
	if err := c.do("GET", "/api/links", nil, &links); err != nil {
		return Link{}, err
	}
	id, idErr := strconv.ParseInt(ref, 10, 64)
	host, path := splitURL(ref)
	for _, l := range links {
		if (idErr == nil && l.ID == id) || (l.Host == host && l.Path == path) {
			return l, nil
		}
	}
	return Link{}, fmt.Errorf("no link %q", ref)
}

func ago(ts *int64) string {
	if ts == nil {
		return "never"
	}
	d := time.Since(time.Unix(*ts, 0)).Round(time.Minute)
	if d < time.Minute {
		return "just now"
	}
	return d.String() + " ago"
}

func runClient(cmd string, args []string) error {
	if cmd == "healthcheck" { // for the container HEALTHCHECK, no wget in scratch
		resp, err := http.Get("http://127.0.0.1" + env("SIGNPOST_ADMIN_LISTEN", ":8081") + "/healthz")
		if err != nil || resp.StatusCode != 200 {
			return errors.New("unhealthy")
		}
		return nil
	}
	c := client{strings.TrimRight(os.Getenv("SIGNPOST_URL"), "/"), os.Getenv("SIGNPOST_TOKEN")}
	if c.base == "" || c.token == "" {
		return errors.New("set SIGNPOST_URL and SIGNPOST_TOKEN")
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	// Flags may come after positionals ("add x y -301"), so pull them out first.
	var pos, flags []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
		} else if n := len(flags); n > 0 && takesValue[strings.TrimLeft(flags[n-1], "-")] {
			flags = append(flags, a)
		} else {
			pos = append(pos, a)
		}
	}

	switch cmd {
	case "ls":
		var links []Link
		if err := c.do("GET", "/api/links", nil, &links); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tLINK\tCODE\tCLICKS\tLAST\tCHECK\tTARGET")
		for _, l := range links {
			fmt.Fprintf(tw, "%d\t%s\t%d\t%d\t%s\t%s\t%s\n", l.ID, l.URL(), l.Status, l.Clicks, ago(l.LastClick), l.Check, l.Target)
		}
		return tw.Flush()

	case "add":
		s301, s307, s308 := fs.Bool("301", false, ""), fs.Bool("307", false, ""), fs.Bool("308", false, "")
		fs.Bool("302", true, "")
		drop := fs.Bool("drop-query", false, "")
		note := fs.String("note", "", "")
		force := fs.Bool("force", false, "")
		short := fs.Bool("short", false, "")
		tags := fs.String("tags", "", "")
		password := fs.String("password", "", "")
		fs.Parse(flags)
		if len(pos) != 2 {
			return errors.New("usage: signpost add <host/path> <target>, or signpost add -short <host> <target>")
		}
		status := 302
		switch {
		case *s301:
			status = 301
		case *s307:
			status = 307
		case *s308:
			status = 308
		}
		host, path := splitURL(pos[0])
		if *short {
			path = "" // the server picks a free slug
		}
		keep := !*drop
		var l Link
		in := linkInput{Host: host, Path: path, Target: pos[1], Status: status, KeepQuery: &keep, Note: *note, Force: *force, Password: *password}
		if *tags != "" {
			in.Tags = []string{*tags}
		}
		if err := c.do("POST", "/api/links", in, &l); err != nil {
			return err
		}
		fmt.Printf("https://%s -> %s (%d). Checking it goes live: `signpost ls` in a bit.\n", l.URL(), l.Target, l.Status)
		return nil

	case "set":
		if len(pos) != 2 {
			return errors.New("usage: signpost set <host/path|id> <target>")
		}
		l, err := c.resolve(pos[0])
		if err != nil {
			return err
		}
		if err := c.do("PATCH", fmt.Sprintf("/api/links/%d", l.ID), map[string]string{"target": pos[1]}, &l); err != nil {
			return err
		}
		fmt.Printf("https://%s -> %s\n", l.URL(), l.Target)
		return nil

	case "rm":
		if len(pos) != 1 {
			return errors.New("usage: signpost rm <host/path|id>")
		}
		l, err := c.resolve(pos[0])
		if err != nil {
			return err
		}
		if err := c.do("DELETE", fmt.Sprintf("/api/links/%d", l.ID), nil, nil); err != nil {
			return err
		}
		fmt.Printf("removed https://%s\n", l.URL())
		return nil

	case "clicks":
		if len(pos) != 1 {
			return errors.New("usage: signpost clicks <host/path|id>")
		}
		l, err := c.resolve(pos[0])
		if err != nil {
			return err
		}
		var cs []Click
		if err := c.do("GET", fmt.Sprintf("/api/links/%d/clicks", l.ID), nil, &cs); err != nil {
			return err
		}
		fmt.Printf("https://%s: %d clicks\n", l.URL(), l.Clicks)
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, x := range cs {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", time.Unix(x.At, 0).Format("Jan 02 15:04"), x.Country, x.Referrer)
		}
		return tw.Flush()

	case "check":
		if len(pos) != 1 {
			return errors.New("usage: signpost check <host/path|id>")
		}
		l, err := c.resolve(pos[0])
		if err != nil {
			return err
		}
		if err := c.do("POST", fmt.Sprintf("/api/links/%d/check", l.ID), nil, nil); err != nil {
			return err
		}
		fmt.Println("re-checking; see `signpost ls`")
		return nil

	case "domains":
		var ds []Domain
		if err := c.do("GET", "/api/domains", nil, &ds); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "HOST\tLINKS\tRESOLVER\tFALLBACK")
		for _, d := range ds {
			fb := d.Fallback
			if fb == "" {
				fb = "-"
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", d.Host, d.Links, d.CertResolver, fb)
		}
		return tw.Flush()

	case "domain":
		resolver := fs.String("resolver", "", "")
		fallback := fs.String("fallback", "", "")
		noFb := fs.Bool("no-fallback", false, "")
		fs.Parse(flags)
		if len(pos) != 1 {
			return errors.New("usage: signpost domain <host> [-resolver name] [-fallback url|-no-fallback]")
		}
		host := strings.ToLower(pos[0])
		var cur Domain
		var ds []Domain
		if err := c.do("GET", "/api/domains", nil, &ds); err != nil {
			return err
		}
		for _, d := range ds {
			if d.Host == host {
				cur = d
			}
		}
		if *resolver != "" {
			cur.CertResolver = *resolver
		}
		if *fallback != "" {
			cur.Fallback = *fallback
		}
		if *noFb {
			cur.Fallback = ""
		}
		if err := c.do("PUT", "/api/domains/"+url.PathEscape(host), cur, &cur); err != nil {
			return err
		}
		fmt.Printf("%s resolver=%s fallback=%q\n", host, cur.CertResolver, cur.Fallback)
		return nil

	case "import":
		dry := fs.Bool("dry-run", false, "")
		force := fs.Bool("force", false, "")
		skip := fs.Bool("skip-check", false, "")
		fs.Parse(flags)
		if len(pos) != 1 {
			return errors.New("usage: signpost import <file.csv|-> [-dry-run] [-force] [-skip-check]")
		}
		var b []byte
		var err error
		if pos[0] == "-" {
			b, err = io.ReadAll(os.Stdin)
		} else {
			b, err = os.ReadFile(pos[0])
		}
		if err != nil {
			return err
		}
		var res struct {
			Created int       `json:"created"`
			Rows    []bulkRow `json:"rows"`
		}
		req := bulkRequest{CSV: string(b), bulkOpts: bulkOpts{DryRun: *dry, Force: *force, SkipProbe: *skip}}
		if err := c.do("POST", "/api/links/bulk", req, &res); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ROW\tLINK\tRESULT\tDETAIL")
		counts := map[string]int{}
		for _, r := range res.Rows {
			counts[r.Result]++
			link := r.Input.Host + r.Input.Path
			if r.Link != nil {
				link = r.Link.URL()
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", r.Row, link, r.Result, r.Msg)
		}
		tw.Flush()
		if *dry {
			fmt.Printf("\ndry run: %d ready, %d conflict, %d duplicate, %d invalid. Nothing was created.\n",
				counts["ok"], counts["conflict"], counts["duplicate"], counts["invalid"])
		} else {
			fmt.Printf("\ncreated %d link(s)\n", res.Created)
		}
		return nil

	case "export":
		req, _ := http.NewRequest("GET", c.base+"/api/links.csv", nil)
		req.Header.Set("Authorization", "Bearer "+c.token)
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return errors.New(resp.Status)
		}
		_, err = io.Copy(os.Stdout, resp.Body)
		return err

	case "domain-rm":
		if len(pos) != 1 {
			return errors.New("usage: signpost domain-rm <host>")
		}
		return c.do("DELETE", "/api/domains/"+url.PathEscape(strings.ToLower(pos[0])), nil, nil)
	}
	fmt.Print(usage)
	return fmt.Errorf("unknown command %q", cmd)
}
