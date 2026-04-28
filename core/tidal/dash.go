package tidal

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var (
	initRegex  = regexp.MustCompile(`initialization="([^"]+)"`)
	mediaRegex = regexp.MustCompile(`media="([^"]+)"`)
	sRegex     = regexp.MustCompile(`<S\s+d="[^"]+"\s*(r="([^"]+)")?\s*/>`)
)

// ExtractDASHStream takes a base64 encoded DASH manifest and returns a sequential reader.
func ExtractDASHStream(ctx context.Context, manifestB64 string, httpClient *http.Client) (io.ReadCloser, string, error) {
	manifest, err := base64.StdEncoding.DecodeString(manifestB64)
	if err != nil {
		return nil, "", fmt.Errorf("invalid base64 manifest: %w", err)
	}
	xmlData := string(manifest)

	mInit := initRegex.FindStringSubmatch(xmlData)
	if len(mInit) < 2 {
		return nil, "", fmt.Errorf("no initialization segment in DASH manifest")
	}

	mMedia := mediaRegex.FindStringSubmatch(xmlData)
	if len(mMedia) < 2 {
		return nil, "", fmt.Errorf("no media segment template in DASH manifest")
	}

	initURL := strings.ReplaceAll(mInit[1], "&amp;", "&")
	mediaURL := strings.ReplaceAll(mMedia[1], "&amp;", "&")

	// Calculate max segments
	maxSeg := 0
	matches := sRegex.FindAllStringSubmatch(xmlData, -1)
	for _, m := range matches {
		maxSeg++ // The segment itself
		if len(m) > 2 && m[2] != "" {
			repeat, _ := strconv.Atoi(m[2])
			maxSeg += repeat
		}
	}
	
	if maxSeg == 0 {
		return nil, "", fmt.Errorf("no segments found in DASH timeline")
	}

	return &dashReader{
		ctx:        ctx,
		client:     httpClient,
		initURL:    initURL,
		mediaURL:   mediaURL,
		currentSeg: 0,
		maxSeg:     maxSeg,
	}, "audio/mp4", nil
}

type dashReader struct {
	ctx        context.Context
	client     *http.Client
	initURL    string
	mediaURL   string
	currentSeg int
	maxSeg     int
	currReader io.ReadCloser
}

func (d *dashReader) Read(p []byte) (n int, err error) {
	for {
		if d.ctx.Err() != nil {
			return 0, d.ctx.Err()
		}

		if d.currReader == nil {
			var url string
			if d.currentSeg == 0 {
				url = d.initURL
			} else if d.currentSeg <= d.maxSeg {
				url = strings.ReplaceAll(d.mediaURL, "$Number$", strconv.Itoa(d.currentSeg))
			} else {
				return 0, io.EOF
			}

			//log.Trace(d.ctx, "Fetching DASH segment", "segment", d.currentSeg, "max", d.maxSeg)
			req, err := http.NewRequestWithContext(d.ctx, "GET", url, nil)
			if err != nil {
				return 0, err
			}

			resp, err := d.client.Do(req)
			if err != nil {
				return 0, err
			}

			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				// If we hit a 404/403 at the end of what we thought was the timeline, treat as EOF
				if d.currentSeg > 0 {
					return 0, io.EOF
				}
				return 0, fmt.Errorf("failed to fetch DASH segment %d: status %d", d.currentSeg, resp.StatusCode)
			}

			d.currReader = resp.Body
		}

		n, err = d.currReader.Read(p)
		if err == io.EOF {
			d.currReader.Close()
			d.currReader = nil
			d.currentSeg++
			if n > 0 {
				return n, nil
			}
			continue // Read from next segment
		}
		return n, err
	}
}

func (d *dashReader) Close() error {
	if d.currReader != nil {
		return d.currReader.Close()
	}
	return nil
}
