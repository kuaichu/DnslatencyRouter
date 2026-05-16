package cloudflare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

const baseURL = "https://api.cloudflare.com/client/v4"

type Client struct {
	apiToken string
	zoneID   string
	recordID string
	http     *http.Client
}

type dnsRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

type apiResponse struct {
	Success  bool            `json:"success"`
	Errors   []apiError      `json:"errors"`
	Result   json.RawMessage `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func New(apiToken, zoneID, recordID, proxyURL string) *Client {
	transport := &http.Transport{
		IdleConnTimeout: 30 * time.Second,
	}

	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err == nil {
			switch u.Scheme {
			case "socks5":
				dialer, err := proxy.SOCKS5("tcp", u.Host, nil, proxy.Direct)
				if err == nil {
					transport.Dial = dialer.Dial
				}
			case "http", "https":
				transport.Proxy = http.ProxyURL(u)
			}
		}
	}

	return &Client{
		apiToken: apiToken,
		zoneID:   zoneID,
		recordID: recordID,
		http: &http.Client{
			Timeout:   15 * time.Second,
			Transport: transport,
		},
	}
}

func (c *Client) do(method, url string, body io.Reader) (*apiResponse, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("parse response (status %d): %w", resp.StatusCode, err)
	}

	return &apiResp, nil
}

// GetRecord fetches the current DNS record.
func (c *Client) GetRecord() (*dnsRecord, error) {
	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", baseURL, c.zoneID, c.recordID)
	apiResp, err := c.do("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if !apiResp.Success {
		return nil, fmt.Errorf("get record: %s", formatErrors(apiResp.Errors))
	}

	var rec dnsRecord
	if err := json.Unmarshal(apiResp.Result, &rec); err != nil {
		return nil, fmt.Errorf("parse record: %w", err)
	}
	return &rec, nil
}

// CurrentIP returns the current record content.
func (c *Client) CurrentIP() (string, error) {
	rec, err := c.GetRecord()
	if err != nil {
		return "", err
	}
	return rec.Content, nil
}

// UpdateRecord updates the DNS record to point to the given IP.
// It first verifies the record exists via GetRecord to preserve its settings.
func (c *Client) UpdateRecord(ip string) error {
	rec, err := c.GetRecord()
	if err != nil {
		return fmt.Errorf("get record before update: %w", err)
	}

	if rec.Content == ip {
		return nil // no change needed
	}

	body := map[string]interface{}{
		"type":    rec.Type,
		"name":    rec.Name,
		"content": ip,
		"ttl":     rec.TTL,
		"proxied": rec.Proxied,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal update body: %w", err)
	}

	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", baseURL, c.zoneID, c.recordID)
	apiResp, err := c.do("PATCH", url, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}
	if !apiResp.Success {
		return fmt.Errorf("update record: %s", formatErrors(apiResp.Errors))
	}

	return nil
}

func formatErrors(errs []apiError) string {
	var s string
	for i, e := range errs {
		if i > 0 {
			s += "; "
		}
		s += fmt.Sprintf("[%d] %s", e.Code, e.Message)
	}
	return s
}
