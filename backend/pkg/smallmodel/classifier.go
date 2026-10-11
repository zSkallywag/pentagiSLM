package smallmodel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Classifier is a client for the zero-shot decision sidecar: it turns a free-text
// situation plus a closed set of labels into a single calibrated choice. It is
// used for the discrete decisions a small generative model is slow and unreliable
// at (picking a container image, allow/deny, success/failure), offloading them to
// a fast deterministic classifier.
type Classifier struct {
	url  string
	http *http.Client
}

// NewClassifier builds a Classifier for the sidecar at url (its base, without a
// path). A zero timeout keeps the agent from stalling if the sidecar is down;
// callers fall back to the model on error.
func NewClassifier(url string) *Classifier {
	return &Classifier{url: url, http: &http.Client{Timeout: 8 * time.Second}}
}

type classifyRequest struct {
	Text   string   `json:"text"`
	Labels []string `json:"labels"`
}

type classifyResponse struct {
	Label string  `json:"label"`
	Score float64 `json:"score"`
}

// Choose returns the best label for text among labels, with its confidence. An
// error means the sidecar was unreachable or replied unusably, and the caller
// should fall back rather than trust a guess.
func (c *Classifier) Choose(ctx context.Context, text string, labels []string) (string, float64, error) {
	payload, err := json.Marshal(classifyRequest{Text: text, Labels: labels})
	if err != nil {
		return "", 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/classify", bytes.NewReader(payload))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("classifier returned status %d", resp.StatusCode)
	}

	var out classifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, err
	}
	if out.Label == "" {
		return "", 0, fmt.Errorf("classifier returned an empty label")
	}
	return out.Label, out.Score, nil
}
