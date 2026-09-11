package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func testAPI(server *httptest.Server) *discordClient {
	d := newDiscordClient("test-token")
	d.baseURL = server.URL
	d.http = server.Client()
	return d
}

func TestMessagesPaginationAndRateLimit(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bot test-token" {
			t.Error("wrong method or auth")
		}
		requests++
		if requests == 1 {
			w.WriteHeader(429)
			fmt.Fprint(w, `{"retry_after":0.001}`)
			return
		}
		before := 206
		if value := r.URL.Query().Get("before"); value != "" {
			before, _ = strconv.Atoi(value)
		}
		var page []message
		for id := before - 1; id >= 1 && len(page) < 100; id-- {
			page = append(page, message{messageBody: messageBody{ID: strconv.Itoa(id), ChannelID: "42", Timestamp: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)}})
		}
		json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()
	messages, err := testAPI(server).messages(context.Background(), "42")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 205 || messages[0].ID != "1" || messages[204].ID != "205" || requests != 4 {
		t.Fatalf("pagination lost messages: count=%d requests=%d", len(messages), requests)
	}
}

func TestThreadPaginationAndPrivateFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/guilds/1/threads/active":
			fmt.Fprint(w, `{"threads":[{"id":"99","parent_id":"unselected"}]}`)
		case "/channels/42/threads/archived/public":
			if r.URL.Query().Get("before") == "" {
				fmt.Fprint(w, `{"threads":[{"id":"100","parent_id":"42","thread_metadata":{"archive_timestamp":"2026-09-10T00:00:00Z"}}],"has_more":true}`)
			} else {
				if r.URL.Query().Get("before") != "2026-09-10T00:00:00Z" {
					t.Error("wrong archive cursor")
				}
				fmt.Fprint(w, `{"threads":[{"id":"90","parent_id":"42"}]}`)
			}
		case "/channels/42/threads/archived/private":
			w.WriteHeader(403)
			fmt.Fprint(w, "{}")
		case "/channels/42/users/@me/threads/archived/private":
			fmt.Fprint(w, `{"threads":[{"id":"80","parent_id":"42"}]}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	got, err := testAPI(server).threads(context.Background(), "1", map[string]bool{"42": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got["42"]) != 3 {
		t.Fatalf("unexpected threads: %#v", got)
	}
}

func TestReactionUserPagination(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/channels/42/messages/99/reactions/custom:55" || r.URL.Query().Get("type") != "1" {
			t.Errorf("wrong reaction request: %s", r.URL)
		}
		after := 0
		if s := r.URL.Query().Get("after"); s != "" {
			after, _ = strconv.Atoi(s)
		}
		page := []user{}
		for id := after + 1; id <= 205 && len(page) < 100; id++ {
			page = append(page, user{ID: strconv.Itoa(id), Username: "Reactor"})
		}
		json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()
	users, err := testAPI(server).reactionUsers(context.Background(), "42", "99", emoji{ID: "55", Name: "custom"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 205 || requests != 3 || users[204].ID != "205" {
		t.Fatal("lost paginated reaction users")
	}
}
