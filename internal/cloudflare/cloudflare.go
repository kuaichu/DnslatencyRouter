package cloudflare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
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

func (c *Client) doRaw(method, url string, body io.Reader) (json.RawMessage, error) {
	apiResp, err := c.do(method, url, body)
	if err != nil {
		return nil, err
	}
	if !apiResp.Success {
		return nil, fmt.Errorf("%s %s: %s", method, url, formatErrors(apiResp.Errors))
	}
	return apiResp.Result, nil
}

// GetRecord fetches the current DNS record.
func (c *Client) GetRecord() (*dnsRecord, error) {
	if strings.TrimSpace(c.recordID) == "" {
		return nil, fmt.Errorf("record_id is empty")
	}
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

func (c *Client) FindARecordByName(name string) (*dnsRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("record name is empty")
	}
	u := fmt.Sprintf("%s/zones/%s/dns_records?type=A&name=%s&per_page=1", baseURL, c.zoneID, url.QueryEscape(name))
	result, err := c.doRaw("GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("find A record %s: %w", name, err)
	}
	var records []dnsRecord
	if err := json.Unmarshal(result, &records); err != nil {
		return nil, fmt.Errorf("parse record search: %w", err)
	}
	if len(records) == 0 {
		return nil, nil
	}
	return &records[0], nil
}

func (c *Client) CreateARecord(name, ip string) (*dnsRecord, error) {
	name = strings.TrimSpace(name)
	ip = strings.TrimSpace(ip)
	if name == "" {
		return nil, fmt.Errorf("record name is empty")
	}
	if ip == "" {
		return nil, fmt.Errorf("record ip is empty")
	}
	body := map[string]interface{}{
		"type":    "A",
		"name":    name,
		"content": ip,
		"ttl":     1,
		"proxied": false,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal create body: %w", err)
	}
	u := fmt.Sprintf("%s/zones/%s/dns_records", baseURL, c.zoneID)
	result, err := c.doRaw("POST", u, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create A record %s: %w", name, err)
	}
	var rec dnsRecord
	if err := json.Unmarshal(result, &rec); err != nil {
		return nil, fmt.Errorf("parse created record: %w", err)
	}
	c.recordID = rec.ID
	return &rec, nil
}

func (c *Client) GetRecordByName(name string) (*dnsRecord, error) {
	if strings.TrimSpace(c.recordID) != "" {
		return c.GetRecord()
	}
	rec, err := c.FindARecordByName(name)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("A record %s does not exist", name)
	}
	c.recordID = rec.ID
	return rec, nil
}

// CurrentIP returns the current record content.
func (c *Client) CurrentIP() (string, error) {
	rec, err := c.GetRecord()
	if err != nil {
		return "", err
	}
	return rec.Content, nil
}

func (c *Client) CurrentIPByName(name string) (string, error) {
	rec, err := c.GetRecordByName(name)
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

func (c *Client) UpdateRecordByName(name, ip string) error {
	rec, err := c.GetRecordByName(name)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			_, createErr := c.CreateARecord(name, ip)
			return createErr
		}
		return fmt.Errorf("get record before update: %w", err)
	}

	if rec.Content == ip {
		return nil
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
	u := fmt.Sprintf("%s/zones/%s/dns_records/%s", baseURL, c.zoneID, rec.ID)
	if _, err := c.doRaw("PATCH", u, bytes.NewReader(jsonBody)); err != nil {
		return fmt.Errorf("update record %s: %w", name, err)
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
