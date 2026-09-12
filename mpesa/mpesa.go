// Package mpesa is a minimal client for Safaricom's Daraja API, covering
// OAuth token retrieval and the Lipa Na M-Pesa Online (STK Push) flow.
// No SDK, no extra dependencies beyond the standard library.
package mpesa

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

type Client struct {
	consumerKey    string
	consumerSecret string
	shortcode      string
	passkey        string
	callbackURL    string
	baseURL        string
	httpClient     *http.Client

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

// NewClientFromEnv builds a Client from MPESA_* environment variables.
// See .env for the full list.
func NewClientFromEnv() *Client {
	baseURL := "https://sandbox.safaricom.co.ke"
	if os.Getenv("MPESA_ENV") == "production" {
		baseURL = "https://api.safaricom.co.ke"
	}
	return &Client{
		consumerKey:    os.Getenv("MPESA_CONSUMER_KEY"),
		consumerSecret: os.Getenv("MPESA_CONSUMER_SECRET"),
		shortcode:      os.Getenv("MPESA_SHORTCODE"),
		passkey:        os.Getenv("MPESA_PASSKEY"),
		callbackURL:    os.Getenv("MPESA_CALLBACK_URL"),
		baseURL:        baseURL,
		httpClient:     &http.Client{Timeout: 15 * time.Second},
	}
}

// accessToken returns a cached OAuth token, refreshing it shortly before
// Safaricom's ~1hr expiry.
func (c *Client) accessToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExpiry) {
		return c.token, nil
	}

	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/oauth/v1/generate?grant_type=client_credentials", nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.consumerKey, c.consumerSecret)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mpesa: oauth failed (%d): %s", resp.StatusCode, body)
	}

	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	c.token = out.AccessToken
	c.tokenExpiry = time.Now().Add(50 * time.Minute) // refresh 10 min early
	return c.token, nil
}

// STKPushResponse mirrors Safaricom's immediate (synchronous) response.
// The actual payment result arrives later via the callback URL — see
// CallbackPayload.
type STKPushResponse struct {
	MerchantRequestID   string `json:"MerchantRequestID"`
	CheckoutRequestID   string `json:"CheckoutRequestID"`
	ResponseCode        string `json:"ResponseCode"`
	ResponseDescription string `json:"ResponseDescription"`
	CustomerMessage     string `json:"CustomerMessage"`
}

// STKPush prompts the given phone (format 2547XXXXXXXX) to pay amount
// (whole KES). accountReference shows up on the STK prompt and in the
// callback, so pass something like "Order42".
func (c *Client) STKPush(phone string, amount int, accountReference, description string) (*STKPushResponse, error) {
	token, err := c.accessToken()
	if err != nil {
		return nil, err
	}

	timestamp := time.Now().Format("20060102150405")
	password := base64.StdEncoding.EncodeToString([]byte(c.shortcode + c.passkey + timestamp))

	payload := map[string]interface{}{
		"BusinessShortCode": c.shortcode,
		"Password":          password,
		"Timestamp":         timestamp,
		"TransactionType":   "CustomerPayBillOnline",
		"Amount":            amount,
		"PartyA":            phone,
		"PartyB":            c.shortcode,
		"PhoneNumber":       phone,
		"CallBackURL":       c.callbackURL,
		"AccountReference":  accountReference,
		"TransactionDesc":   description,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/mpesa/stkpush/v1/processrequest", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mpesa: stk push failed (%d): %s", resp.StatusCode, respBody)
	}

	var out STKPushResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, err
	}
	if out.ResponseCode != "0" {
		return nil, fmt.Errorf("mpesa: %s", out.ResponseDescription)
	}
	return &out, nil
}

// CallbackPayload is the body Safaricom POSTs to your callback URL once
// the customer enters their PIN (or cancels / times out).
type CallbackPayload struct {
	Body struct {
		StkCallback struct {
			MerchantRequestID string `json:"MerchantRequestID"`
			CheckoutRequestID string `json:"CheckoutRequestID"`
			ResultCode        int    `json:"ResultCode"`
			ResultDesc        string `json:"ResultDesc"`
			CallbackMetadata  struct {
				Item []struct {
					Name  string      `json:"Name"`
					Value interface{} `json:"Value"`
				} `json:"Item"`
			} `json:"CallbackMetadata"`
		} `json:"stkCallback"`
	} `json:"Body"`
}

// MpesaReceipt pulls "MpesaReceiptNumber" out of a successful callback's
// metadata, if present (absent on failed/cancelled payments).
func (p *CallbackPayload) MpesaReceipt() string {
	for _, item := range p.Body.StkCallback.CallbackMetadata.Item {
		if item.Name == "MpesaReceiptNumber" {
			if s, ok := item.Value.(string); ok {
				return s
			}
		}
	}
	return ""
}