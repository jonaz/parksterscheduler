package main

import (
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

const (
	pushoverURL = "https://api.pushover.net/1/messages.json"
	programName = "parksterScheduler"
)

// Notifier sends push notifications.
type Notifier interface {
	Info(message string)
	Error(message string)
}

type pushover struct {
	apiKey, userKey string
	http            *http.Client
}

func newPushover(apiKey, userKey string) *pushover {
	return &pushover{apiKey: apiKey, userKey: userKey, http: &http.Client{Timeout: 10 * time.Second}}
}

func (p *pushover) Info(message string) {
	log.Printf("ℹ️ %s", message)
	p.send(programName, message)
}

func (p *pushover) Error(message string) {
	log.Printf("❌ %s", message)
	p.send("Error in "+programName, message)
}

func (p *pushover) send(title, message string) {
	if p.apiKey == "" || p.userKey == "" {
		return // Do nothing if credentials are not provided.
	}
	resp, err := p.http.PostForm(pushoverURL, url.Values{
		"token":   {p.apiKey},
		"user":    {p.userKey},
		"title":   {title},
		"message": {message},
	})
	if err != nil {
		log.Printf("🚨 Pushover notification failed to send: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("🚨 Pushover API returned non-200 status: %s. Body: %s", resp.Status, string(body))
	}
}
