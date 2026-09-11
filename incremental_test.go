package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestIncrementalSnapshotAndRestart(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	makeMessage := func(cid string, ago time.Duration, body string) message {
		ts := now.Add(-ago)
		return message{messageBody: messageBody{ID: snowflakeAt(ts), ChannelID: cid, Timestamp: ts, Content: body}, Author: user{ID: "12", Username: "Alice"}}
	}
	ancient := makeMessage("42", 10*24*time.Hour, "older content")
	starter := makeMessage("42", 9*24*time.Hour, "thread starter")
	deleted := makeMessage("42", 2*time.Hour, "delete me")
	edited := makeMessage("42", time.Hour, "before edit")
	edited.Reactions = []reaction{{Emoji: emoji{Name: "wave"}, Count: 1}}
	reply := makeMessage(starter.ID, 8*24*time.Hour, "old reply")
	newReply := makeMessage(starter.ID, 30*time.Minute, "new reply")
	added := makeMessage("42", 10*time.Minute, "new message")
	phase := 0
	requests := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests[r.URL.Path]++
		if r.Method != "GET" {
			t.Error("unexpected write")
		}
		switch r.URL.Path {
		case "/channels/42", "/channels/43":
			id := strings.Split(r.URL.Path, "/")[2]
			json.NewEncoder(w).Encode(channel{ID: id, GuildID: "1", Name: "channel " + strconv.Itoa(phase), Type: 0})
		case "/users/@me":
			fmt.Fprint(w, `{"id":"7","bot":true}`)
		case "/guilds/1/members/7":
			fmt.Fprint(w, `{"roles":[]}`)
		case "/guilds/1/roles":
			fmt.Fprint(w, `[{"id":"1","permissions":"66560"}]`)
		case "/guilds/1/threads/active":
			ts := []channel{}
			if phase < 2 {
				ts = append(ts, channel{ID: starter.ID, ParentID: "42", Name: "thread"})
			}
			json.NewEncoder(w).Encode(threadPage{Threads: ts})
		default:
			if strings.Contains(r.URL.Path, "/threads/archived/") {
				fmt.Fprint(w, `{"threads":[]}`)
				return
			}
			if strings.Contains(r.URL.Path, "/reactions/") {
				id := "14"
				if phase > 0 {
					id = "15"
				}
				json.NewEncoder(w).Encode([]user{{ID: id, Username: "reactor"}})
				return
			}
			if !strings.HasSuffix(r.URL.Path, "/messages") {
				t.Errorf("unexpected %s", r.URL.Path)
				w.WriteHeader(404)
				return
			}
			if phase == 3 {
				w.WriteHeader(403)
				return
			}
			id := strings.Split(r.URL.Path, "/")[2]
			var ms []message
			switch id {
			case "42":
				ms = []message{ancient, starter, deleted, edited}
				if phase > 0 {
					changed := edited
					changed.Content = "after edit"
					changed.Author.Username = "Alice renamed"
					oldChanged := ancient
					oldChanged.Content = "old edit"
					ms = []message{oldChanged, starter, changed, added}
				}
			case "43":
				ms = []message{makeMessage("43", 100*24*time.Hour, "new channel backfill")}
			default:
				ms = []message{reply}
				if phase > 0 {
					ms = append(ms, newReply)
				}
			}
			if phase > 0 {
				for i := range ms {
					ms[i].Author.Username = "Alice renamed"
				}
			}
			sort.Slice(ms, func(i, j int) bool { return idLess(ms[j].ID, ms[i].ID) })
			json.NewEncoder(w).Encode(ms)
		}
	}))
	defer server.Close()
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "archive")
	c := config{GuildID: "1", Channels: []channelConfig{{ID: "42"}}, Branch: "discord"}
	if err := initRepo(ctx, c, repo); err != nil {
		t.Fatal(err)
	}
	api := testAPI(server)
	if _, _, err := syncOnce(ctx, api, c, repo); err != nil {
		t.Fatal(err)
	}
	// Simulate a process dying halfway through replacement, before commit.
	if err := os.Remove(filepath.Join(repo, "channels", "42", "messages", ancient.ID, "data.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "partial-write"), []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	phase = 1
	c.Channels = append(c.Channels, channelConfig{ID: "43"})
	// A new cache is loaded from disk on every call: this is also the restart path.
	rev, _, err := syncIncrementalOnce(ctx, api, c, repo, now.Add(-24*time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	path := func(cid, mid string) string {
		return filepath.Join(repo, "channels", cid, "messages", mid, "data.json")
	}
	if m := readJSONTest[archivedMessage](t, path("42", ancient.ID)); m.Content != "older content" {
		t.Fatal("old range was fetched instead of retained")
	}
	if _, err := os.Stat(path("42", deleted.ID)); !os.IsNotExist(err) {
		t.Fatal("recent deletion retained")
	}
	m := readJSONTest[archivedMessage](t, path("42", edited.ID))
	if m.Content != "after edit" || len(m.Reactions[0].NormalUserIDs) != 1 || m.Reactions[0].NormalUserIDs[0] != "15" {
		t.Fatal("edit or same-count reactor change missed")
	}
	if _, err := os.Stat(path("42", added.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path("43", snowflakeAt(now.Add(-100*24*time.Hour)))); err != nil {
		t.Fatal("new channel was not backfilled", err)
	}
	threadPath := filepath.Join(repo, "channels", "42", "messages", starter.ID, newReply.ID, "data.json")
	if _, err := os.Stat(threadPath); err != nil {
		t.Fatal(err)
	}
	if u := readJSONTest[user](t, filepath.Join(repo, "users", "12", "data.json")); u.Username != "Alice renamed" {
		t.Fatal("user rename missed")
	}
	// Do not publish another commit when the snapshot is unchanged after restart.
	again, _, err := syncIncrementalOnce(ctx, api, c, repo, now.Add(-24*time.Hour), false)
	if err != nil || again != rev {
		t.Fatalf("unchanged restart: %s %v", again, err)
	}
	phase = 2
	if _, _, err := syncIncrementalOnce(ctx, api, c, repo, now.Add(-24*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(threadPath); !os.IsNotExist(err) {
		t.Fatal("deleted thread retained")
	}
	if m := readJSONTest[archivedMessage](t, path("42", starter.ID)); m.Thread != nil {
		t.Fatal("stale thread metadata")
	}
	// A full reconciliation detects changes outside the overlap.
	last, _, err := syncIncrementalOnce(ctx, api, c, repo, now.Add(-24*time.Hour), true)
	if err != nil {
		t.Fatal(err)
	}
	if m := readJSONTest[archivedMessage](t, path("42", ancient.ID)); m.Content != "old edit" {
		t.Fatal("full reconciliation missed old edit")
	}
	phase = 3
	if _, _, err := syncIncrementalOnce(ctx, api, c, repo, now.Add(-24*time.Hour), false); err == nil {
		t.Fatal("expected failure")
	}
	head, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil || head != last {
		t.Fatal("failed incremental scan advanced checkpoint")
	}
}

func TestIncrementalPaginationAndLongOutage(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	oldID := snowflakeAt(now.Add(-10 * 24 * time.Hour))
	cutoff := snowflakeAt(now.Add(-24 * time.Hour))
	old := archivedMessage{messageBody: messageBody{ID: oldID, ChannelID: "42", Timestamp: now.Add(-10 * 24 * time.Hour)}, AuthorID: "12"}
	cache := &snapshotCache{messages: map[string]map[string]archivedMessage{"42": {oldID: old}}, cutoff: now.Add(-24 * time.Hour)}
	if !idLess(oldID, cache.lowerBound("42")) {
		t.Fatal("long outage would lose messages")
	}
	if cache.lowerBound("43") != "" {
		t.Fatal("new stream must backfill")
	}
	boundary := &snapshotCache{messages: map[string]map[string]archivedMessage{"42": {cutoff: {}}}, cutoff: now.Add(-24 * time.Hour)}
	if boundary.lowerBound("42") != cutoff {
		t.Fatal("overlap boundary must be inclusive")
	}
	// >100 messages after an old cursor must all be fetched, including those
	// older than the recent-edit window. Older cached messages are retained.
	var all []message
	for i := 0; i < 250; i++ {
		ts := now.Add(-5 * 24 * time.Hour).Add(time.Duration(i) * time.Minute)
		all = append(all, message{messageBody: messageBody{ID: snowflakeAt(ts), ChannelID: "42", Timestamp: ts}, Author: user{ID: "12"}})
	}
	sort.Slice(all, func(i, j int) bool { return idLess(all[j].ID, all[i].ID) })
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		before := r.URL.Query().Get("before")
		page := []message{}
		for _, m := range all {
			if before == "" || idLess(m.ID, before) {
				page = append(page, m)
			}
			if len(page) == 100 {
				break
			}
		}
		json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()
	api := testAPI(server)
	result, err := refreshMessages(context.Background(), api, "42", map[string]user{}, cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 251 || calls != 3 {
		t.Fatalf("got %d messages in %d requests", len(result), calls)
	}
	if !idLess(result[0].ID, cutoff) {
		t.Fatal("test did not cover outage gap")
	}
	calls = 0
	// A recent checkpoint stops old pagination after one page.
	ms, err := api.messagesFrom(context.Background(), "42", all[4].ID)
	if err != nil || len(ms) != 5 || calls != 1 {
		t.Fatalf("bounded read: %d %d %v", len(ms), calls, err)
	}
}
