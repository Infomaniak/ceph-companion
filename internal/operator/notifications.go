package operator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/infomaniak/ceph-companion/internal/config"
)

// NotificationClient sends notifications to a webhook (e.g. kChat).
type NotificationClient struct {
	config *config.NotificationConfig
	client *http.Client
}

// NewNotificationClient creates a new NotificationClient.
func NewNotificationClient(cfg *config.NotificationConfig) *NotificationClient {
	return &NotificationClient{
		config: cfg,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Send sends a message to the configured notification webhook.
func (c *NotificationClient) Send(message string) error {
	return c.send(c.config.Channel, c.config.Webhook, message)
}

// SendError sends an execution-error notification, preferring the dedicated
// error channel/webhook (spam-tolerant channel) when configured and falling
// back to the regular notification target otherwise.
func (c *NotificationClient) SendError(message string) error {
	channel, webhook := c.config.Channel, c.config.Webhook
	if c.config.ErrorChannel != "" {
		channel = c.config.ErrorChannel
	}
	if c.config.ErrorWebhook != "" {
		webhook = c.config.ErrorWebhook
	}
	return c.send(channel, webhook, message)
}

func (c *NotificationClient) send(channel, webhook, message string) error {
	if c.config == nil || webhook == "" {
		return fmt.Errorf("notifications not configured")
	}

	payload := map[string]interface{}{
		"channel":    channel,
		"username":   c.config.Username,
		"icon_emoji": c.config.Emoji,
		"text":       message,
	}

	if payload["username"] == "" {
		payload["username"] = "ceph-companion"
	}
	if payload["icon_emoji"] == "" {
		payload["icon_emoji"] = ":ceph1:"
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", webhook, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("notification webhook returned %d", resp.StatusCode)
	}
	return nil
}
