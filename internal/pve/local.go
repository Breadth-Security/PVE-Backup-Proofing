package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// NewLocal builds a Client that reads PVE through pvesh on the node itself
// (as root, no token): every GET runs
// `<pvesh> get <path> [--<k> <v>...] --output-format json`. Other methods
// are refused. The error run returns for a failed command must carry the
// command's stderr (pvesh reports API errors there); callers match on it.
func NewLocal(pvesh string, run func(ctx context.Context, name string, args []string) (stdout []byte, err error)) *Client {
	return &Client{
		local: func(ctx context.Context, path string, query url.Values) ([]byte, error) {
			p, err := url.PathUnescape(path)
			if err != nil {
				return nil, fmt.Errorf("pvesh %s: %w", path, err)
			}
			args := []string{"get", p}
			keys := make([]string, 0, len(query))
			for k := range query {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				for _, v := range query[k] {
					args = append(args, "--"+k, v)
				}
			}
			args = append(args, "--output-format", "json")
			out, err := run(ctx, pvesh, args)
			if err != nil {
				return nil, fmt.Errorf("pvesh get %s: %w", p, err)
			}
			return out, nil
		},
		pollInterval: defaultPollEvery,
		retryBackoff: defaultRetryBackoff,
	}
}

// doLocal serves do for a pvesh-backed client. pvesh prints the bare data
// payload, not the {"data": ...} envelope the HTTP API wraps it in.
func (c *Client) doLocal(ctx context.Context, method, path string, query url.Values, out any) error {
	if method != http.MethodGet {
		return fmt.Errorf("pvesh client: %s %s: only GET is supported", method, path)
	}
	raw, err := c.local(ctx, path, query)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), out); err != nil {
		return fmt.Errorf("pvesh get %s: decode data: %w", path, err)
	}
	return nil
}
