package cloudflare

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d", status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    &http.Request{},
	}
}

func testClient(rt http.RoundTripper) *Client {
	return &Client{
		apiToken: "token",
		zoneID:   "zone",
		recordID: "cached-for-another-name",
		http:     &http.Client{Transport: rt},
	}
}

func TestGetRecordByNameDoesNotReuseRecordIDForAnotherName(t *testing.T) {
	var requests []string
	c := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Method+" "+req.URL.RequestURI())
		if req.URL.Path != "/client/v4/zones/zone/dns_records" {
			t.Fatalf("unexpected path: %s", req.URL.Path)
		}
		return jsonResponse(http.StatusOK, `{"success":true,"result":[{"id":"record-b","type":"A","name":"b.example.com","content":"192.0.2.2","ttl":120,"proxied":false}]}`), nil
	}))

	rec, err := c.GetRecordByName("b.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID != "record-b" || len(requests) != 1 {
		t.Fatalf("record=%#v requests=%v", rec, requests)
	}
}

func TestUpdateRecordByNameDoesNotCreateAfterTemporaryFailure(t *testing.T) {
	var requests []string
	c := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Method+" "+req.URL.RequestURI())
		return jsonResponse(http.StatusBadGateway, `{"success":false,"errors":[{"code":1000,"message":"temporary upstream failure; record does not exist yet"}]}`), nil
	}))

	err := c.UpdateRecordByName("b.example.com", "8.8.8.9")
	if err == nil {
		t.Fatal("temporary API failure unexpectedly succeeded")
	}
	if len(requests) != 1 {
		t.Fatalf("temporary failure triggered create/update requests: %v", requests)
	}
	if errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("temporary failure classified as not found: %v", err)
	}
}

func TestUpdateRecordByNameCreatesOnlyForNotFound(t *testing.T) {
	var requests []string
	c := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Method+" "+req.URL.RequestURI())
		if req.Method == http.MethodGet {
			return jsonResponse(http.StatusNotFound, `{"success":false,"errors":[{"code":81044,"message":"Record does not exist"}]}`), nil
		}
		return jsonResponse(http.StatusOK, `{"success":true,"result":{"id":"created","type":"A","name":"b.example.com","content":"192.0.2.9","ttl":1,"proxied":false}}`), nil
	}))

	if err := c.UpdateRecordByName("b.example.com", "8.8.8.9"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || !strings.HasPrefix(requests[1], "POST ") {
		t.Fatalf("not-found update requests = %v", requests)
	}
}

func TestGetRecordByNameEmptyResultIsTypedNotFound(t *testing.T) {
	c := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"success":true,"result":[]}`), nil
	}))
	_, err := c.GetRecordByName("missing.example.com")
	if !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("error = %v, want ErrRecordNotFound", err)
	}
}

func TestUpdateRecordByNameFailureDoesNotCreateOrDelete(t *testing.T) {
	var methods []string
	c := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		methods = append(methods, req.Method)
		switch req.Method {
		case http.MethodGet:
			return jsonResponse(http.StatusOK, `{"success":true,"result":[{"id":"record-b","type":"A","name":"b.example.com","content":"192.0.2.2","ttl":120,"proxied":false}]}`), nil
		case http.MethodPatch:
			return jsonResponse(http.StatusServiceUnavailable, `{"success":false,"errors":[{"code":1000,"message":"temporary update failure"}]}`), nil
		default:
			return jsonResponse(http.StatusInternalServerError, `{"success":false,"errors":[{"code":1000,"message":"unexpected write"}]}`), nil
		}
	}))

	err := c.UpdateRecordByName("b.example.com", "8.8.8.9")
	if err == nil {
		t.Fatal("patch failure unexpectedly succeeded")
	}
	if got := strings.Join(methods, ","); got != "GET,PATCH" {
		t.Fatalf("methods after patch failure = %s, want GET,PATCH", got)
	}
}

func TestGetRecordWithoutIDIsNotFound(t *testing.T) {
	c := &Client{}
	_, err := c.GetRecord()
	if !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("error = %v, want ErrRecordNotFound", err)
	}
}

func TestCreateAndUpdateRejectNonPublicIPv4(t *testing.T) {
	c := &Client{}
	for _, ip := range []string{"10.0.0.1", "127.0.0.1", "203.0.113.2", "2001:4860:4860::8888", "not-an-ip"} {
		if _, err := c.CreateARecord("route.example.com", ip); err == nil {
			t.Errorf("CreateARecord accepted %q", ip)
		}
		if err := c.UpdateRecord(ip); err == nil {
			t.Errorf("UpdateRecord accepted %q", ip)
		}
		if err := c.UpdateRecordByName("route.example.com", ip); err == nil {
			t.Errorf("UpdateRecordByName accepted %q", ip)
		}
	}
}

func TestPublicIPv4IsCanonicalizedBeforeCreate(t *testing.T) {
	var body string
	c := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(req.Body)
		body = string(data)
		return jsonResponse(http.StatusOK, `{"success":true,"result":{"id":"created","type":"A","name":"route.example.com","content":"8.8.8.8","ttl":1,"proxied":false}}`), nil
	}))
	if _, err := c.CreateARecord("route.example.com", " 8.8.8.8 "); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"content":"8.8.8.8"`) {
		t.Fatalf("create body did not contain canonical IP: %s", body)
	}
}

func TestGetRecordRejectsNonAType(t *testing.T) {
	c := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"success":true,"result":{"id":"record","type":"AAAA","name":"route.example.com","content":"2001:db8::1","ttl":120,"proxied":false}}`), nil
	}))
	if _, err := c.GetRecord(); err == nil || !strings.Contains(err.Error(), "expected A") {
		t.Fatalf("non-A record error = %v", err)
	}
}

func TestHTTPErrorIsReturnedEvenWhenJSONSuccess(t *testing.T) {
	c := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadGateway, `{"success":true,"result":[]}`), nil
	}))
	if _, err := c.FindARecordByName("route.example.com"); err == nil || !strings.Contains(err.Error(), "HTTP status 502") {
		t.Fatalf("HTTP error was accepted: %v", err)
	}
}
