package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func backoff(attempt int) time.Duration {
	return time.Duration(min(2000, 200<<min(attempt, 4))) * time.Millisecond
}
func coordinatorRequest(client *http.Client, method, target string, data []byte) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(data))
	if err != nil {
		cancel()
		return nil, err
	}
	if data != nil {
		req.Header.Set("content-type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	res.Body = &cancelBody{ReadCloser: res.Body, cancel: cancel}
	return res, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error { defer b.cancel(); return b.ReadCloser.Close() }
func announce(client *http.Client, origin string, port int) error {
	data := jsonBytes(obj("port", port))
	for attempt := 0; ; attempt++ {
		res, err := coordinatorRequest(client, "POST", origin+"/register", data)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 409 {
				return nil
			}
			if res.StatusCode == 400 || res.StatusCode == 405 {
				return fmt.Errorf("Registration rejected with HTTP %d", res.StatusCode)
			}
		}
		time.Sleep(backoff(attempt))
	}
}
func awaitCompletion(client *http.Client, origin string) {
	for attempt := 0; ; attempt++ {
		res, err := coordinatorRequest(client, "GET", origin+"/done", nil)
		if err == nil {
			var status map[string]any
			if res.StatusCode >= 200 && res.StatusCode < 300 {
				var data []byte
				data, err = io.ReadAll(res.Body)
				if err == nil {
					err = json.Unmarshal(data, &status)
				}
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if err == nil && status != nil && status["done"] == true {
				return
			}
		}
		time.Sleep(backoff(attempt))
	}
}
func run() error {
	origin := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if origin == "" {
		return fmt.Errorf("MAKEFUN_ENV_URL is not set")
	}
	server, err := startForgeServer()
	if err != nil {
		return err
	}
	client := &http.Client{}
	if err = announce(client, origin, server.Port()); err != nil {
		return err
	}
	awaitCompletion(client, origin)
	return server.Close()
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
