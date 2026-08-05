package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

var ClientBackend = newRawClient(dialBackend)

type RawClient struct {
	client  func() *http.Client
	timeout time.Duration
}

func newRawClient(dialer func(ctx context.Context) (net.Conn, error)) *RawClient {
	client := &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer(ctx)
			},
		},
	}

	return &RawClient{
		timeout: 10 * time.Second,
		client: func() *http.Client {
			return client
		},
	}
}

func (c *RawClient) Get(ctx context.Context, endpoint string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+endpoint, http.NoBody)
	if err != nil {
		return err
	}

	response, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	buf, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}

	if response.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("GET %s: %s", endpoint, response.Status)
	}

	if err := json.Unmarshal(buf, &v); err != nil {
		return err
	}
	return nil
}

func (c *RawClient) Post(ctx context.Context, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost"+endpoint, http.NoBody)
	if err != nil {
		return err
	}

	response, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)

	if response.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("POST %s: %s", endpoint, response.Status)
	}
	return nil
}
