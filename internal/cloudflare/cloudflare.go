package cloudflare

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dns-latency-router/internal/checker"
	"golang.org/x/net/proxy"
)

// ErrRecordNotFound identifies a missing DNS record. Callers can use
// errors.Is to distinguish a missing record from transient API failures.
var ErrRecordNotFound = errors.New("cloudflare record not found")

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
	status  int
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func isRecordNotFound(resp *apiResponse) bool {
	if resp == nil {
		return false
	}
	if resp.status == http.StatusNotFound {
		return true
	}
	for _, apiErr := range resp.Errors {
		// Cloudflare uses 81044 for a DNS record that does not exist.
		if apiErr.Code == 81044 {
			return true
		}
	}
	return false
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
	apiResp.status = resp.StatusCode
	if resp.StatusCode >= http.StatusBadRequest {
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("cloudflare HTTP status %d: %w", resp.StatusCode, ErrRecordNotFound)
		}
		return nil, fmt.Errorf("cloudflare HTTP status %d: %s", resp.StatusCode, formatErrors(apiResp.Errors))
	}

	return &apiResp, nil
}

func (c *Client) doRaw(method, url string, body io.Reader) (json.RawMessage, error) {
	apiResp, err := c.do(method, url, body)
	if err != nil {
		return nil, err
	}
	if !apiResp.Success {
		if isRecordNotFound(apiResp) {
			return nil, fmt.Errorf("%w: %s", ErrRecordNotFound, formatErrors(apiResp.Errors))
		}
		return nil, fmt.Errorf("%s %s: %s", method, url, formatErrors(apiResp.Errors))
	}
	return apiResp.Result, nil
}

// GetRecord fetches the current DNS record.
func (c *Client) GetRecord() (*dnsRecord, error) {
	if strings.TrimSpace(c.recordID) == "" {
		return nil, fmt.Errorf("record_id is empty: %w", ErrRecordNotFound)
	}
	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", baseURL, c.zoneID, c.recordID)
	apiResp, err := c.do("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if !apiResp.Success {
		if isRecordNotFound(apiResp) {
			return nil, fmt.Errorf("get record: %w", ErrRecordNotFound)
		}
		return nil, fmt.Errorf("get record: %s", formatErrors(apiResp.Errors))
	}

	var rec dnsRecord
	if err := json.Unmarshal(apiResp.Result, &rec); err != nil {
		return nil, fmt.Errorf("parse record: %w", err)
	}
	if err := validateARecord(rec); err != nil {
		return nil, err
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
	if err := validateARecord(records[0]); err != nil {
		return nil, err
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
	ip, ok := checker.NormalizeCandidateIP(ip)
	if !ok {
		return nil, fmt.Errorf("record ip must be a public IPv4 address")
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
	if err := validateARecord(rec); err != nil {
		return nil, err
	}
	c.recordID = rec.ID
	return &rec, nil
}

func (c *Client) GetRecordByName(name string) (*dnsRecord, error) {
	rec, err := c.FindARecordByName(name)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("%w: A record %s does not exist", ErrRecordNotFound, name)
	}
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
	normalizedIP, ok := checker.NormalizeCandidateIP(ip)
	if !ok {
		return fmt.Errorf("record ip must be a public IPv4 address")
	}
	rec, err := c.GetRecord()
	if err != nil {
		return fmt.Errorf("get record before update: %w", err)
	}

	if rec.Content == normalizedIP {
		return nil // no change needed
	}

	body := map[string]interface{}{
		"type":    rec.Type,
		"name":    rec.Name,
		"content": normalizedIP,
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
	normalizedIP, ok := checker.NormalizeCandidateIP(ip)
	if !ok {
		return fmt.Errorf("record ip must be a public IPv4 address")
	}
	rec, err := c.GetRecordByName(name)
	if err != nil {
		if errors.Is(err, ErrRecordNotFound) {
			_, createErr := c.CreateARecord(name, ip)
			return createErr
		}
		return fmt.Errorf("get record before update: %w", err)
	}

	if rec.Content == normalizedIP {
		return nil
	}

	body := map[string]interface{}{
		"type":    rec.Type,
		"name":    rec.Name,
		"content": normalizedIP,
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

func validateARecord(rec dnsRecord) error {
	if !strings.EqualFold(strings.TrimSpace(rec.Type), "A") {
		return fmt.Errorf("cloudflare record %s has type %q; expected A", rec.ID, rec.Type)
	}
	return nil
}

func (c *Client) DeleteRecord() error {
	rec, err := c.GetRecord()
	if err != nil {
		return fmt.Errorf("get record before delete: %w", err)
	}
	u := fmt.Sprintf("%s/zones/%s/dns_records/%s", baseURL, c.zoneID, rec.ID)
	if _, err := c.doRaw("DELETE", u, nil); err != nil {
		return fmt.Errorf("delete record: %w", err)
	}
	if rec.ID == c.recordID {
		c.recordID = ""
	}
	return nil
}

func (c *Client) DeleteRecordByName(name string) error {
	rec, err := c.GetRecordByName(name)
	if err != nil {
		if errors.Is(err, ErrRecordNotFound) {
			return nil
		}
		return fmt.Errorf("get record before delete: %w", err)
	}
	u := fmt.Sprintf("%s/zones/%s/dns_records/%s", baseURL, c.zoneID, rec.ID)
	if _, err := c.doRaw("DELETE", u, nil); err != nil {
		return fmt.Errorf("delete record %s: %w", name, err)
	}
	if rec.ID == c.recordID {
		c.recordID = ""
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
