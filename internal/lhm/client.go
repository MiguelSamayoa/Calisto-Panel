package lhm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const maxBody = 8 << 20

// Reading is one parsed LHM sensor value, raw and unformatted.
type Reading struct {
	Value float64
	Unit  string
}

type node struct {
	Value    string `json:"Value"`
	RawValue string `json:"RawValue"`
	SensorID string `json:"SensorId"`
	Children []node `json:"Children"`
}

// Client polls the LHM remote web server.
type Client struct {
	url     string
	timeout time.Duration
	http    *http.Client
}

func NewClient(url string, timeout time.Duration) *Client {
	return &Client{url: url, timeout: timeout, http: &http.Client{}}
}

// Fetch returns every leaf sensor that has a parseable value, keyed by SensorId.
// Sensors with "-" or otherwise unparseable values are omitted, which the caller
// treats the same as a missing sensor.
func (c *Client) Fetch(ctx context.Context) (map[string]Reading, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("building lhm request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting lhm data: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lhm returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("reading lhm body: %w", err)
	}
	return Parse(body)
}

// Parse flattens an LHM data.json document. When a SensorId appears more than once
// (LHM reuses some ids), the first occurrence wins.
func Parse(data []byte) (map[string]Reading, error) {
	var root node
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &root); err != nil {
		return nil, fmt.Errorf("decoding lhm data: %w", err)
	}
	out := map[string]Reading{}
	flatten(root, out)
	return out, nil
}

func flatten(n node, out map[string]Reading) {
	if n.SensorID != "" {
		if _, dup := out[n.SensorID]; !dup {
			raw := n.RawValue
			if raw == "" {
				raw = n.Value
			}
			if v, unit, err := ParseValue(raw); err == nil {
				out[n.SensorID] = Reading{Value: v, Unit: unit}
			}
		}
	}
	for _, c := range n.Children {
		flatten(c, out)
	}
}
