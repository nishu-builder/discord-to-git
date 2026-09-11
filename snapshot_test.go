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

func TestSnapshotGitRoundTripAndFailure(t *testing.T) {
	phase := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/channels/42":
			fmt.Fprint(w, `{"id":"42","guild_id":"1","name":"general","type":0}`)
		case "/users/@me":
			fmt.Fprint(w, `{"id":"7","bot":true}`)
		case "/guilds/1/members/7":
			fmt.Fprint(w, `{"roles":[]}`)
		case "/guilds/1/roles":
			fmt.Fprint(w, `[{"id":"1","permissions":"66560"}]`)
		case "/guilds/1/threads/active":
			if phase == 0 {
				fmt.Fprint(w, `{"threads":[{"id":"99","name":"Question / answer","parent_id":"42"}]}`)
			} else {
				fmt.Fprint(w, `{"threads":[]}`)
			}
		case "/channels/42/threads/archived/public", "/channels/42/threads/archived/private":
			fmt.Fprint(w, `{"threads":[]}`)
		case "/channels/42/messages", "/channels/99/messages":
			if phase == 2 {
				w.WriteHeader(403)
				fmt.Fprint(w, "{}")
				return
			}
			cid := "42"
			if strings.Contains(r.URL.Path, "99") {
				cid = "99"
			}
			content := "hello from the archive"
			if phase == 1 {
				content = "edited message"
			}
			m := message{ID: "1000", ChannelID: cid, Timestamp: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC), Content: content, Author: user{ID: "12", Username: "Alice"}}
			json.NewEncoder(w).Encode([]message{m})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	if _, err := git(ctx, root, "init", "--bare", remote); err != nil {
		t.Fatal(err)
	}
	c := config{GuildID: "1", Folder: "Team", Channels: []channelConfig{{ID: "42", Folder: "general"}}, Branch: "discord", Remote: remote}
	repo := filepath.Join(root, "archive")
	if err := initRepo(ctx, c, repo); err != nil {
		t.Fatal(err)
	}
	api := testAPI(server)
	first, count, err := syncOnce(ctx, api, c, repo)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count %d", count)
	}
	threadPath := filepath.Join(repo, "Team/general/threads/99/2026-09-10.md")
	if body, err := os.ReadFile(threadPath); err != nil || !strings.Contains(string(body), "hello from the archive") {
		t.Fatalf("thread missing: %v", err)
	}
	second, _, err := syncOnce(ctx, api, c, repo)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("unchanged snapshot created a commit")
	}
	phase = 1
	third, _, err := syncOnce(ctx, api, c, repo)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("edit did not create a new commit")
	}
	if _, err := os.Stat(threadPath); !os.IsNotExist(err) {
		t.Fatal("deleted thread survived")
	}
	body, err := os.ReadFile(filepath.Join(repo, "Team/general/2026-09-10.md"))
	if err != nil || !strings.Contains(string(body), "edited message") {
		t.Fatal("edit missing")
	}
	// A permission failure must not publish a partial or empty replacement.
	phase = 2
	if _, _, err := syncOnce(ctx, api, c, repo); err == nil {
		t.Fatal("expected permission error")
	}
	head, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil || head != third {
		t.Fatal("failed refresh changed Git HEAD")
	}
	pushed, err := git(ctx, remote, "rev-parse", "refs/heads/discord")
	if err != nil || pushed != third {
		t.Fatal("push did not reach remote")
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
		`[{"id":"42","folder":"../escape"}]`,
		`[{"id":"42","folder":"a"},{"id":"42","folder":"b"}]`,
		`[{"id":"42","folder":"a"},{"id":"43","folder":"a"}]`,
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		body := `{"guild_id":"1","folder":"Team","channels":` + channels + "}"
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil {
			t.Fatal("accepted unsafe config")
		}
	}
}
