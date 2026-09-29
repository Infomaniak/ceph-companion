package prometheus

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var debug = os.Getenv("DEBUG") == "1"

// Client represents a Prometheus HTTP client.
type Client struct {
	HTTPClient *http.Client
}

// NewClient creates a new Prometheus client.
func NewClient() *Client {
	return &Client{
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Auth holds optional basic-auth credentials for a Prometheus endpoint
// secured behind a reverse proxy. A nil *Auth means no authentication.
type Auth struct {
	Username string
	Password string
}

// NewAuth returns nil when username is empty, so callers can pass the
// configured credentials straight through without special-casing the
// unsecured-endpoint case.
func NewAuth(username, password string) *Auth {
	if username == "" {
		return nil
	}
	return &Auth{Username: username, Password: password}
}

// Query executes a Prometheus query and returns results.
func (c *Client) Query(baseURL, query string, auth *Auth) ([]PrometheusResult, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	// Preserve any subpath in the configured URL (e.g. a VictoriaMetrics
	// endpoint served under /prometheus) and avoid double-appending when
	// the caller already passes a full query URL (see QueryURL).
	if !strings.HasSuffix(u.Path, "/api/v1/query") {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/api/v1/query"
	}

	q := u.Query()
	q.Set("query", query)
	u.RawQuery = q.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "Ceph-Companion/1.0")
	if auth != nil {
		req.SetBasicAuth(auth.Username, auth.Password)
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			lastErr = err
			if attempt < 2 {
				time.Sleep(2 * time.Second)
				continue
			}
			return nil, fmt.Errorf("query failed after retries: %w", lastErr)
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			if attempt < 2 {
				time.Sleep(2 * time.Second)
				continue
			}
			return nil, err
		}

		if debug {
			fmt.Fprintf(os.Stderr, "HTTP %d - %s\n", resp.StatusCode, u.String())
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			if attempt < 2 {
				time.Sleep(2 * time.Second)
				continue
			}
			return nil, lastErr
		}

		var result PrometheusResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("invalid JSON response: %w", err)
		}

		if result.Status != "success" {
			return nil, fmt.Errorf("prometheus error: %s", result.Status)
		}

		return result.Data.Result, nil
	}

	return nil, lastErr
}

// BuildSelector joins label matchers into a Prometheus selector fragment,
// e.g. BuildSelector([2]string{"cluster", "abc"}, [2]string{"pool_id", "1"})
// returns "{cluster='abc',pool_id='1'}". Matchers with an empty value are
// skipped, and "" is returned if none remain.
func BuildSelector(matchers ...[2]string) string {
	var parts []string
	for _, m := range matchers {
		if m[1] == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s='%s'", m[0], m[1]))
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// FirstLabel returns the first non-empty value among the given label keys.
func FirstLabel(metric map[string]string, keys []string) string {
	for _, k := range keys {
		if v := metric[k]; v != "" {
			return v
		}
	}
	return ""
}

// ExtractValue extracts a float value from a Prometheus result.
func ExtractValue(r PrometheusResult) float64 {
	if len(r.Value) >= 2 {
		if s, ok := r.Value[1].(string); ok {
			v, _ := strconv.ParseFloat(s, 64)
			return v
		}
	}
	return 0
}

// ComputeDrop computes percentage drop between short-term and long-term values.
func ComputeDrop(shortTerm, longTerm float64) float64 {
	if longTerm > 0 {
		return (shortTerm/longTerm - 1) * 100
	}
	return 0
}

// FormatRate formats a per-second rate with human-readable units.
func FormatRate(value float64, unit string) string {
	switch unit {
	case "bytes":
		if value >= 1e9 {
			return fmt.Sprintf("%.2f GB/s", value/1e9)
		} else if value >= 1e6 {
			return fmt.Sprintf("%.2f MB/s", value/1e6)
		}
		return fmt.Sprintf("%.2f B/s", value)
	case "queries":
		if value >= 1e9 {
			return fmt.Sprintf("%.2f G/s", value/1e9)
		} else if value >= 1e6 {
			return fmt.Sprintf("%.2f M/s", value/1e6)
		} else if value >= 1e3 {
			return fmt.Sprintf("%.2f K/s", value/1e3)
		}
		return fmt.Sprintf("%.2f /s", value)
	}
	return fmt.Sprintf("%.2f", value)
}
