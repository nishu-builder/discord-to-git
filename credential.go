package main

import (
	"bufio"
	"context"
	"errors"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

// Normal use reads an environment variable. The optional socket lets a broker
// inject a token into a running service without putting it in a file or unit.
func readToken(ctx context.Context, socketPath string) (string, error) {
	if socketPath == "" {
		token := strings.TrimSpace(os.Getenv("DISCORD_BOT_TOKEN"))
		os.Unsetenv("DISCORD_BOT_TOKEN") // Child Git processes do not need this credential.
		if token == "" {
			return "", errors.New("set DISCORD_BOT_TOKEN or use -token-socket")
		}
		return token, nil
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return "", err
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0600); err != nil {
		return "", err
	}
	log.Print("waiting for broker credential on private Unix socket")
	go func() { <-ctx.Done(); listener.Close() }()
	conn, err := listener.Accept()
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return "", err
	}
	line, err := bufio.NewReaderSize(conn, 4096).ReadSlice('\n')
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(line))
	if token == "" {
		return "", errors.New("empty broker credential")
	}
	return token, nil
}
