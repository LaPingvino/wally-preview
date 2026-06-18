package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

var errUnauthorized = errors.New("unauthorized")

// matrixClient talks to the local homeserver for the two things the shim needs
// it for: validating the caller's access token (so we are not an open fetch
// proxy) and uploading the og:image to mint a spec-compliant mxc:// URI.
//
// Its http.Client is a PLAIN one — it connects to the homeserver, which is on
// localhost and would be rejected by the SSRF-guarded client. Keep the two
// clients strictly separate.
type matrixClient struct {
	homeserver  string
	uploadToken string
	hc          *http.Client
}

func (m *matrixClient) whoami(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		m.homeserver+"/_matrix/client/v3/account/whoami", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := m.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", errUnauthorized
	}
	var out struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", err
	}
	if out.UserID == "" {
		return "", errUnauthorized
	}
	return out.UserID, nil
}

// uploadMedia stores the preview image in the homeserver's media repo using the
// dedicated service token and returns the resulting mxc:// URI.
func (m *matrixClient) uploadMedia(ctx context.Context, data []byte, contentType string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.homeserver+"/_matrix/media/v3/upload", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+m.uploadToken)
	req.Header.Set("Content-Type", contentType)
	resp, err := m.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("media upload: status %d", resp.StatusCode)
	}
	var out struct {
		ContentURI string `json:"content_uri"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", err
	}
	if out.ContentURI == "" {
		return "", errors.New("media upload: empty content_uri")
	}
	return out.ContentURI, nil
}
