package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type emailGateway struct {
	Host, APIKey, Sender, Recipient string
	Client                          *http.Client
}

func (g emailGateway) enabled() bool {
	return g.Host != "" && g.APIKey != "" && g.Sender != "" && g.Recipient != ""
}

func (g emailGateway) sendQuestion(ctx context.Context, q question, ip string) error {
	if !g.enabled() {
		return nil
	}
	from := q.Name
	if from == "" {
		from = "Anonymous"
	}
	message := fmt.Sprintf("New message from %s\nEmail: %s\n\n%s", from, q.Email, q.Message)
	payload, err := json.Marshal(map[string]string{
		"from": g.Sender, "to": g.Recipient, "subject": "The Honest Question: new message",
		"text_body": message, "reply_to": q.Email,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(g.Host, "/")+"/v1/email", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+g.APIKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if ip != "" {
		request.Header.Set("X-Egate-Client-IP", ip)
	}
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("egate returned HTTP %d", response.StatusCode)
	}
	return nil
}
