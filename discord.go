package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

type threadPage struct {
	Threads []channel `json:"threads"`
	HasMore bool      `json:"has_more"`
}
type apiError struct {
	Path   string
	Status int
}

func (e *apiError) Error() string { return fmt.Sprintf("Discord GET %s: HTTP %d", e.Path, e.Status) }

type discordClient struct {
	baseURL     string
	token       string
	http        *http.Client
	nextRequest time.Time
}

func newDiscordClient(token string) *discordClient {
	return &discordClient{baseURL: "https://discord.com/api/v10", token: token, http: &http.Client{
		Timeout: 30 * time.Second,
		// Never forward a bot credential to a redirect destination.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(max(d, 0))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Requests are sequential. Respect advisory bucket resets and explicit 429 retries.
func (d *discordClient) get(ctx context.Context, path string, target any) error {
	for attempt := 0; attempt < 5; attempt++ {
		if err := wait(ctx, time.Until(d.nextRequest)); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+d.token)
		req.Header.Set("User-Agent", "DiscordBot (https://github.com/nishu-builder/discord-to-git, 0.1)")
		response, err := d.http.Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
		response.Body.Close()
		if err != nil {
			return err
		}
		// Also stay below Discord's global bot request rate.
		d.nextRequest = time.Now().Add(50 * time.Millisecond)
		if response.Header.Get("X-RateLimit-Remaining") == "0" {
			seconds, err := strconv.ParseFloat(response.Header.Get("X-RateLimit-Reset-After"), 64)
			if err != nil {
				return fmt.Errorf("invalid Discord rate limit header: %w", err)
			}
			d.nextRequest = time.Now().Add(time.Duration(seconds*float64(time.Second)) + 100*time.Millisecond)
		}
		if response.StatusCode == 429 {
			var rate struct {
				RetryAfter float64 `json:"retry_after"`
			}
			if err := json.Unmarshal(body, &rate); err != nil {
				return err
			}
			d.nextRequest = time.Now().Add(time.Duration(rate.RetryAfter*float64(time.Second)) + 100*time.Millisecond)
			continue
		}
		if response.StatusCode >= 500 {
			d.nextRequest = time.Now().Add(time.Duration(1<<attempt) * time.Second)
			continue
		}
		if response.StatusCode != 200 {
			return &apiError{path, response.StatusCode}
		}
		if err := json.Unmarshal(body, target); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		return nil
	}
	return fmt.Errorf("Discord GET %s: retry limit reached", path)
}

func (d *discordClient) checkBot(ctx context.Context) error {
	var bot user
	if err := d.get(ctx, "/users/@me", &bot); err != nil {
		return err
	}
	if !bot.Bot {
		return errors.New("a Discord bot token is required")
	}
	var app struct {
		Flags uint64 `json:"flags"`
	}
	if err := d.get(ctx, "/applications/@me", &app); err != nil {
		return err
	}
	if app.Flags&((1<<18)|(1<<19)) == 0 {
		return errors.New("enable Message Content Intent on the Discord application")
	}
	return nil
}

// Lack of Read Message History returns an empty list, not necessarily an error.
// Check effective permissions so a revoked permission cannot erase the archive.
func (d *discordClient) checkPermissions(ctx context.Context, guild string, channels []channel) error {
	var bot user
	if err := d.get(ctx, "/users/@me", &bot); err != nil {
		return err
	}
	var member struct {
		Roles []string `json:"roles"`
	}
	if err := d.get(ctx, "/guilds/"+guild+"/members/"+bot.ID, &member); err != nil {
		return err
	}
	var roles []struct {
		ID          string `json:"id"`
		Permissions string `json:"permissions"`
	}
	if err := d.get(ctx, "/guilds/"+guild+"/roles", &roles); err != nil {
		return err
	}
	mine := map[string]bool{}
	for _, id := range member.Roles {
		mine[id] = true
	}
	var base uint64
	for _, role := range roles {
		if role.ID == guild || mine[role.ID] {
			bits, err := strconv.ParseUint(role.Permissions, 10, 64)
			if err != nil {
				return err
			}
			base |= bits
		}
	}
	if base&8 != 0 {
		return nil
	} // Administrator bypasses channel overwrites.
	for _, ch := range channels {
		p := base
		var everyoneAllow, everyoneDeny, roleAllow, roleDeny, userAllow, userDeny uint64
		for _, o := range ch.Overwrites {
			a, err := strconv.ParseUint(o.Allow, 10, 64)
			if err != nil {
				return err
			}
			n, err := strconv.ParseUint(o.Deny, 10, 64)
			if err != nil {
				return err
			}
			switch {
			case o.ID == guild:
				everyoneAllow |= a
				everyoneDeny |= n
			case o.Type == 0 && mine[o.ID]:
				roleAllow |= a
				roleDeny |= n
			case o.Type == 1 && o.ID == bot.ID:
				userAllow |= a
				userDeny |= n
			}
		}
		p = (p &^ everyoneDeny) | everyoneAllow
		p = (p &^ roleDeny) | roleAllow
		p = (p &^ userDeny) | userAllow
		if p&(1<<10) == 0 || p&(1<<16) == 0 {
			return fmt.Errorf("%s: bot needs View Channel and Read Message History", ch.Name)
		}
	}
	return nil
}

func (d *discordClient) messages(ctx context.Context, id string) ([]message, error) {
	return d.messagesFrom(ctx, id, "")
}

// Discord returns newest first. Stop once a complete page crosses the inclusive checkpoint.
func (d *discordClient) messagesFrom(ctx context.Context, id, lower string) ([]message, error) {
	var all []message
	before := ""
	for {
		path := "/channels/" + id + "/messages?limit=100"
		if before != "" {
			path += "&before=" + before
		}
		var page []message
		if err := d.get(ctx, path, &page); err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		next := page[len(page)-1].ID
		if !idPattern.MatchString(next) || (before != "" && !idLess(next, before)) {
			return nil, errors.New("message pagination did not advance")
		}
		for _, m := range page {
			if m.ChannelID != id || !idPattern.MatchString(m.ID) || m.Timestamp.IsZero() {
				return nil, errors.New("invalid message in Discord response")
			}
		}
		for _, m := range page {
			if lower == "" || !idLess(m.ID, lower) {
				all = append(all, m)
			}
		}
		if lower != "" && idLess(next, lower) {
			break
		}
		before = next
		if len(page) < 100 {
			break
		}
	}
	sort.Slice(all, func(i, j int) bool { return idLess(all[i].ID, all[j].ID) })
	return all, nil
}

func idLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func (d *discordClient) archived(ctx context.Context, parent, route string, joined bool) ([]channel, error) {
	var all []channel
	before := ""
	for {
		path := "/channels/" + parent + route + "?limit=100"
		if before != "" {
			path += "&before=" + url.QueryEscape(before)
		}
		var page threadPage
		if err := d.get(ctx, path, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Threads...)
		if !page.HasMore {
			return all, nil
		}
		if len(page.Threads) == 0 {
			return nil, errors.New("empty thread page claims more results")
		}
		last := page.Threads[len(page.Threads)-1]
		next := ""
		if last.ThreadMetadata != nil {
			next = last.ThreadMetadata.ArchiveTimestamp
		}
		if joined {
			next = last.ID
		}
		if next == "" || next == before {
			return nil, errors.New("thread pagination did not advance")
		}
		before = next
	}
}

func (d *discordClient) threads(ctx context.Context, guild string, parents map[string]bool) (map[string][]channel, error) {
	var active threadPage
	if err := d.get(ctx, "/guilds/"+guild+"/threads/active", &active); err != nil {
		return nil, err
	}
	byID := map[string]channel{}
	for _, t := range active.Threads {
		if parents[t.ParentID] {
			byID[t.ID] = t
		}
	}
	for parent := range parents {
		public, err := d.archived(ctx, parent, "/threads/archived/public", false)
		if err != nil {
			return nil, err
		}
		private, err := d.archived(ctx, parent, "/threads/archived/private", false)
		var denied *apiError
		if errors.As(err, &denied) && denied.Status == 403 {
			// Bots without Manage Threads can still read private threads they joined.
			private, err = d.archived(ctx, parent, "/users/@me/threads/archived/private", true)
		}
		if err != nil {
			return nil, err
		}
		for _, t := range append(public, private...) {
			if t.ParentID != parent || !idPattern.MatchString(t.ID) {
				return nil, errors.New("unexpected thread parent or ID")
			}
			byID[t.ID] = t
		}
	}
	result := map[string][]channel{}
	for _, t := range byID {
		if !idPattern.MatchString(t.ID) {
			return nil, errors.New("invalid thread ID")
		}
		result[t.ParentID] = append(result[t.ParentID], t)
	}
	for parent := range result {
		sort.Slice(result[parent], func(i, j int) bool { return idLess(result[parent][i].ID, result[parent][j].ID) })
	}
	return result, nil
}

// Each emoji can have normal and burst reactions, with separately paginated users.
func (d *discordClient) reactionUsers(ctx context.Context, channelID, messageID string, e emoji, kind int) ([]user, error) {
	key := e.Name
	if e.ID != "" {
		if !idPattern.MatchString(e.ID) {
			return nil, errors.New("invalid reaction emoji ID")
		}
		key += ":" + e.ID
	}
	if key == "" {
		return nil, errors.New("reaction has no emoji")
	}
	all := []user{}
	after := ""
	for {
		path := "/channels/" + channelID + "/messages/" + messageID + "/reactions/" + url.PathEscape(key) + "?limit=100&type=" + strconv.Itoa(kind)
		if after != "" {
			path += "&after=" + after
		}
		var page []user
		if err := d.get(ctx, path, &page); err != nil {
			return nil, err
		}
		sort.Slice(page, func(i, j int) bool { return idLess(page[i].ID, page[j].ID) })
		for _, u := range page {
			if !idPattern.MatchString(u.ID) || (after != "" && !idLess(after, u.ID)) {
				return nil, errors.New("reaction user pagination did not advance")
			}
			after = u.ID
			all = append(all, u)
		}
		if len(page) < 100 {
			return all, nil
		}
	}
}
