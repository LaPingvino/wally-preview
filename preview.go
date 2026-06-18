package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

var errNoPreview = errors.New("no preview available")

// ogData is the raw scrape result before the image is fetched and uploaded.
type ogData struct {
	title       string
	description string
	siteName    string
	imageURL    string // absolute http(s) URL to the candidate image
	imageW      int
	imageH      int
}

// previewResult is the Matrix `preview_url` response body (IPreviewUrlResponse).
type previewResult struct {
	Title       string `json:"og:title,omitempty"`
	Description string `json:"og:description,omitempty"`
	SiteName    string `json:"og:site_name,omitempty"`
	Image       string `json:"og:image,omitempty"` // mxc://
	ImageW      int    `json:"og:image:width,omitempty"`
	ImageH      int    `json:"og:image:height,omitempty"`
	ImageType   string `json:"og:image:type,omitempty"`
	ImageSize   int    `json:"matrix:image:size,omitempty"`
}

func assignMeta(d *ogData, key, content string) {
	if content == "" {
		return
	}
	switch key {
	case "og:title":
		if d.title == "" {
			d.title = content
		}
	case "og:description", "description":
		if d.description == "" {
			d.description = content
		}
	case "og:site_name":
		if d.siteName == "" {
			d.siteName = content
		}
	case "og:image", "og:image:url", "og:image:secure_url", "twitter:image", "twitter:image:src":
		if d.imageURL == "" {
			d.imageURL = content
		}
	case "og:image:width":
		d.imageW = atoi(content)
	case "og:image:height":
		d.imageH = atoi(content)
	}
}

// parseHTML extracts OpenGraph/Twitter/<title> metadata. It uses the low-level
// tokenizer (no full DOM) and stops at <body>, since the tags we want live in
// <head>. The candidate image URL is resolved against base.
func parseHTML(r io.Reader, base *url.URL) ogData {
	var d ogData
	var titleText string
	var inTitle bool
	z := html.NewTokenizer(r)
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "title":
				inTitle = true
			case "meta":
				var prop, nm, content string
				for hasAttr {
					var k, v []byte
					k, v, hasAttr = z.TagAttr()
					switch strings.ToLower(string(k)) {
					case "property":
						prop = strings.ToLower(strings.TrimSpace(string(v)))
					case "name":
						nm = strings.ToLower(strings.TrimSpace(string(v)))
					case "content":
						content = string(v)
					}
				}
				key := prop
				if key == "" {
					key = nm
				}
				assignMeta(&d, key, content)
			case "body":
				goto done
			}
		case html.TextToken:
			if inTitle && titleText == "" {
				titleText = strings.TrimSpace(string(z.Text()))
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if string(name) == "title" {
				inTitle = false
			}
		}
	}
done:
	if d.title == "" {
		d.title = titleText
	}
	if d.imageURL != "" && base != nil {
		if iu, err := base.Parse(d.imageURL); err == nil {
			d.imageURL = iu.String()
		}
	}
	return d
}

// fetchDoc fetches the target. If it is an HTML document, it returns the scraped
// ogData. If the target is itself an image, it returns the image bytes directly
// (so a bare image link still previews). All fetches use the SSRF-guarded client.
func (s *server) fetchDoc(ctx context.Context, u *url.URL) (og ogData, imgData []byte, imgType string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return ogData{}, nil, "", err
	}
	req.Header.Set("User-Agent", s.cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,image/*;q=0.8,*/*;q=0.5")
	resp, err := s.fetch.Do(req)
	if err != nil {
		return ogData{}, nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ogData{}, nil, "", fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	mt := mediaType(resp.Header.Get("Content-Type"))
	switch {
	case strings.HasPrefix(mt, "image/"):
		data, err := readCapped(resp.Body, s.cfg.MaxImageBytes)
		if err != nil {
			return ogData{}, nil, "", err
		}
		return ogData{imageURL: u.String()}, data, mt, nil
	case mt == "text/html" || mt == "application/xhtml+xml":
		lr := io.LimitReader(resp.Body, s.cfg.MaxHTMLBytes)
		cr, err := charset.NewReader(lr, resp.Header.Get("Content-Type"))
		if err != nil {
			cr = lr
		}
		return parseHTML(cr, u), nil, "", nil
	default:
		return ogData{}, nil, "", errNoPreview
	}
}

// fetchImage downloads an og:image candidate through the guarded client.
func (s *server) fetchImage(ctx context.Context, u *url.URL) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", s.cfg.UserAgent)
	req.Header.Set("Accept", "image/*")
	resp, err := s.fetch.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("image status %d", resp.StatusCode)
	}
	mt := mediaType(resp.Header.Get("Content-Type"))
	if !strings.HasPrefix(mt, "image/") {
		return nil, "", errors.New("og:image is not an image")
	}
	data, err := readCapped(resp.Body, s.cfg.MaxImageBytes)
	if err != nil {
		return nil, "", err
	}
	return data, mt, nil
}

// buildPreview performs the full scrape→image→upload flow and returns the
// spec-shaped response. Returns errNoPreview when there is nothing worth showing.
func (s *server) buildPreview(ctx context.Context, u *url.URL) (*previewResult, error) {
	og, imgData, imgType, err := s.fetchDoc(ctx, u)
	if err != nil {
		return nil, err
	}
	pr := &previewResult{
		Title:       trunc(og.title, 500),
		Description: trunc(og.description, 1000),
		SiteName:    trunc(og.siteName, 200),
		ImageW:      og.imageW,
		ImageH:      og.imageH,
	}

	if imgData == nil && og.imageURL != "" {
		if iu, perr := url.Parse(og.imageURL); perr == nil && (iu.Scheme == "http" || iu.Scheme == "https") {
			if d, t, ferr := s.fetchImage(ctx, iu); ferr == nil {
				imgData, imgType = d, t
			}
		}
	}

	if len(imgData) > 0 {
		if cfg, _, derr := image.DecodeConfig(bytes.NewReader(imgData)); derr == nil {
			pr.ImageW, pr.ImageH = cfg.Width, cfg.Height
		}
		if mxc, uerr := s.mx.uploadMedia(ctx, imgData, imgType); uerr == nil {
			pr.Image = mxc
			pr.ImageType = imgType
			pr.ImageSize = len(imgData)
		}
	}

	if pr.Title == "" && pr.Description == "" && pr.Image == "" {
		return nil, errNoPreview
	}
	return pr, nil
}

func mediaType(ct string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
}

func readCapped(r io.Reader, max int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, max))
}

func trunc(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	if n < 0 {
		return 0
	}
	return n
}
