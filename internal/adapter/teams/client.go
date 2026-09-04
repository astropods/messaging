package teams

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/infracloudio/msbotbuilder-go/connector/client"
	"github.com/infracloudio/msbotbuilder-go/schema"
)

// noAuthClient sends activities without acquiring an Azure AD token.
// msbotbuilder-go's ConnectorClient always fetches one via client_credentials
// before every send, even with empty credentials — a real network round trip
// to Microsoft's login endpoint per message. Used only when TeamsAppID is
// empty (local dev against Microsoft 365 Agents Playground).
type noAuthClient struct {
	http *http.Client

	// devHostOverride replaces a "localhost"/"127.0.0.1" target host.
	// Agents Playground reports its own serviceUrl as localhost regardless
	// of how the bot reached it, which breaks when this adapter runs in a
	// container (ast dev's Compose): localhost there means the container,
	// not the host Agents Playground runs on.
	devHostOverride string
}

func newNoAuthClient(devHostOverride string) *noAuthClient {
	return &noAuthClient{
		http:            &http.Client{Timeout: 10 * time.Second},
		devHostOverride: devHostOverride,
	}
}

func (c *noAuthClient) Post(target url.URL, act schema.Activity) error {
	return c.send(http.MethodPost, target, &act)
}

func (c *noAuthClient) Put(target url.URL, act schema.Activity) error {
	return c.send(http.MethodPut, target, &act)
}

func (c *noAuthClient) Delete(target url.URL, act schema.Activity) error {
	return c.send(http.MethodDelete, target, nil)
}

func (c *noAuthClient) send(method string, target url.URL, act *schema.Activity) error {
	if c.devHostOverride != "" {
		host := target.Hostname()
		if host == "localhost" || host == "127.0.0.1" {
			if port := target.Port(); port != "" {
				target.Host = c.devHostOverride + ":" + port
			} else {
				target.Host = c.devHostOverride
			}
		}
	}

	var body *bytes.Reader
	if act != nil {
		b, err := json.Marshal(act)
		if err != nil {
			return err
		}
		b = stripEmptySuggestedActions(b)
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}

	req, err := http.NewRequest(method, target.String(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Agents Playground's transcript renderer requires this header to be
	// present, even unauthenticated, or it silently drops the reply.
	req.Header.Set("Authorization", "Bearer unauthenticated-local-dev")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("teams: %s %s: status %d", method, target.String(), resp.StatusCode)
	}
	return nil
}

// SuggestedActions is a non-pointer struct so Go always serializes it as
// {}, even though its own (empty) Actions slice is dropped by omitempty.
// Microsoft 365 Agents Playground's renderer assumes the key only appears
// with a populated array and crashes reading .actions.length otherwise,
// silently dropping the reply despite a 201 response. Falls back to the
// original bytes on any parse error.
func stripEmptySuggestedActions(body []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	raw, ok := obj["suggestedActions"]
	if !ok {
		return body
	}
	var suggestedActions map[string]json.RawMessage
	if err := json.Unmarshal(raw, &suggestedActions); err != nil {
		return body
	}
	if _, hasActions := suggestedActions["actions"]; hasActions {
		return body
	}
	delete(obj, "suggestedActions")
	patched, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return patched
}

// skipEmptyClient no-ops a Post of a zero-value activity, since
// ProcessActivity always POSTs whatever the handler returns but this
// adapter's real replies arrive later via ProactiveMessage.
type skipEmptyClient struct {
	client.Client
}

func (c skipEmptyClient) Post(target url.URL, act schema.Activity) error {
	if act.Type == "" {
		return nil
	}
	return c.Client.Post(target, act)
}
