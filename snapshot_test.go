package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readJSONTest[T any](t *testing.T, path string) T {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value T
	if err := json.Unmarshal(b, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestSnapshotGitRoundTripAndFailure(t *testing.T) {
	phase := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("unexpected mutation")
		}
		switch r.URL.Path {
		case "/channels/42":
			name := "general"
			if phase > 0 {
				name = "renamed channel / path"
			}
			json.NewEncoder(w).Encode(channel{ID: "42", GuildID: "1", Name: name, Type: 0})
		case "/users/@me":
			fmt.Fprint(w, `{"id":"7","bot":true}`)
		case "/guilds/1/members/7":
			fmt.Fprint(w, `{"roles":[]}`)
		case "/guilds/1/roles":
			fmt.Fprint(w, `[{"id":"1","permissions":"66560"}]`)
		case "/guilds/1/threads/active":
			if phase < 2 {
				fmt.Fprint(w, `{"threads":[{"id":"99","name":"Question","parent_id":"42"},{"id":"88","name":"Missing starter","parent_id":"42"},{"id":"77","name":"Thread-local starter","parent_id":"42"}]}`)
			} else {
				fmt.Fprint(w, `{"threads":[]}`)
			}
		case "/channels/42/threads/archived/public", "/channels/42/threads/archived/private":
			fmt.Fprint(w, `{"threads":[]}`)
		case "/channels/42/messages", "/channels/99/messages", "/channels/88/messages", "/channels/77/messages":
			if phase == 3 {
				w.WriteHeader(403)
				return
			}
			cid := strings.Split(r.URL.Path, "/")[2]
			id := map[string]string{"42": "99", "99": "1000", "88": "1001", "77": "77"}[cid]
			name := "Alice"
			if phase > 0 {
				name = "Alice renamed"
			}
			content := "hello <@13>\nsecond line"
			if phase >= 2 {
				content = "edited message"
			}
			m := message{
				messageBody: messageBody{ID: id, ChannelID: cid, Timestamp: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC), Content: content,
					Attachments: []json.RawMessage{json.RawMessage(`{"id":"555","filename":"notes.txt","url":"https://example.com/a","size":5}`)},
					Reactions:   []reaction{{Emoji: emoji{ID: "55", Name: "custom"}, Count: 1}}},
				Author: user{ID: "12", Username: name}, Mentions: []user{{ID: "13", Username: "Bob"}}}
			m.Reactions[0].CountDetails.Normal = 1
			m.Reactions[0].CountDetails.Burst = 1
			m.Reactions[0].Count = 2
			json.NewEncoder(w).Encode([]message{m})
		default:
			if strings.Contains(r.URL.Path, "/reactions/") {
				if phase == 4 {
					w.WriteHeader(403)
					return
				}
				if r.URL.Query().Get("type") == "1" {
					fmt.Fprint(w, `[{"id":"15","username":"Burst reactor"}]`)
				} else {
					fmt.Fprint(w, `[{"id":"14","username":"Normal reactor"}]`)
				}
			} else {
				t.Errorf("unexpected request %s", r.URL.Path)
				w.WriteHeader(404)
			}
		}
	}))
	defer server.Close()
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	if _, err := git(ctx, root, "init", "--bare", remote); err != nil {
		t.Fatal(err)
	}
	c := config{GuildID: "1", Channels: []channelConfig{{ID: "42"}}, Branch: "discord", Remote: remote}
	repo := filepath.Join(root, "archive")
	if err := initRepo(ctx, c, repo); err != nil {
		t.Fatal(err)
	}
	api := testAPI(server)
	first, count, err := syncOnce(ctx, api, c, repo)
	if err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("count %d", count)
	}
	threadPath := filepath.Join(repo, "channels/42/messages/99/1000/data.json")
	reply := readJSONTest[archivedMessage](t, threadPath)
	if reply.AuthorID != "12" || len(reply.MentionIDs) != 1 || reply.MentionIDs[0] != "13" {
		t.Fatal("user references lost")
	}
	if !strings.Contains(reply.Content, "<@13>") || len(reply.Attachments) != 1 {
		t.Fatal("message data lost")
	}
	r := reply.Reactions[0]
	if len(r.NormalUserIDs) != 1 || r.NormalUserIDs[0] != "14" || len(r.BurstUserIDs) != 1 || r.BurstUserIDs[0] != "15" {
		t.Fatal("reactor references lost")
	}
	for _, id := range []string{"12", "13", "14", "15"} {
		if u := readJSONTest[user](t, filepath.Join(repo, "users", id, "data.json")); u.ID != id {
			t.Fatal("missing user record")
		}
	}
	starterPath := filepath.Join(repo, "channels/42/messages/99/data.json")
	starter := readJSONTest[archivedMessage](t, starterPath)
	if starter.Missing || starter.Thread == nil || starter.AuthorID != "12" {
		t.Fatal("starter overwritten")
	}
	placeholder := readJSONTest[map[string]any](t, filepath.Join(repo, "channels/42/messages/88/data.json"))
	if placeholder["missing"] != true || placeholder["thread"] == nil || placeholder["author_id"] != nil || placeholder["timestamp"] != nil {
		t.Fatal("invalid missing starter placeholder")
	}
	if _, err := os.Stat(filepath.Join(repo, "channels/42/messages/88/1001/data.json")); err != nil {
		t.Fatal(err)
	}
	threadLocal := readJSONTest[archivedMessage](t, filepath.Join(repo, "channels/42/messages/77/data.json"))
	if threadLocal.Missing || threadLocal.AuthorID != "12" || threadLocal.Thread == nil {
		t.Fatal("thread-local starter lost")
	}
	second, _, err := syncOnce(ctx, api, c, repo)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("unchanged snapshot created a commit")
	}
	phase = 1
	renamed, _, err := syncOnce(ctx, api, c, repo)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := git(ctx, repo, "diff", "--name-only", first, renamed)
	if err != nil {
		t.Fatal(err)
	}
	if changed != "README.md\nchannels/42/data.json\nusers/12/data.json" {
		t.Fatalf("renames changed message files: %s", changed)
	}
	phase = 2
	third, _, err := syncOnce(ctx, api, c, repo)
	if err != nil {
		t.Fatal(err)
	}
	if third == renamed {
		t.Fatal("edit did not create a new commit")
	}
	if _, err := os.Stat(threadPath); !os.IsNotExist(err) {
		t.Fatal("deleted thread survived")
	}
	if m := readJSONTest[archivedMessage](t, starterPath); m.Content != "edited message" || m.Thread != nil {
		t.Fatal("edit or removed thread metadata missing")
	}
	for _, failure := range []int{3, 4} {
		phase = failure
		if _, _, err := syncOnce(ctx, api, c, repo); err == nil {
			t.Fatal("expected permission error")
		}
		head, err := git(ctx, repo, "rev-parse", "HEAD")
		if err != nil || head != third {
			t.Fatal("failed refresh changed Git HEAD")
		}
		pushed, err := git(ctx, remote, "rev-parse", "refs/heads/discord")
		if err != nil || pushed != third {
			t.Fatal("remote changed on failed refresh")
		}
	}
}

func TestRefusesHumanCheckout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("human work"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := initRepo(context.Background(), config{Branch: "discord"}, dir); err == nil {
		t.Fatal("must refuse an existing directory")
	}
}

func TestRejectsUnsafeOrDuplicateConfig(t *testing.T) {
	for _, channels := range []string{
		`[{"id":"../escape"}]`,
		`[{"id":"42"},{"id":"42"}]`,
		`[{"id":"42","folder":"obsolete"}]`,
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		body := `{"guild_id":"1","channels":` + channels + "}"
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil {
			t.Fatal("accepted unsafe or obsolete config")
		}
	}
}
