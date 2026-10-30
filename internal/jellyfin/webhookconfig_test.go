package jellyfin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const pluginID = "71552a5a5c5c4350a2aeebe451a30173"

// fakeWebhookPlugin is a Jellyfin with the Webhook plugin, holding its configuration the
// way the plugin does: as one JSON document, POSTed whole.
type fakeWebhookPlugin struct {
	status string         // the plugin's Status; "" means no plugin at all
	conf   map[string]any // the stored configuration
	drop   bool           // answer the POST but keep the old configuration
	posts  int
}

func (f *fakeWebhookPlugin) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), `Token="key"`) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/Plugins":
			list := []map[string]string{{"Id": "x", "Name": "Playback Reporting", "Version": "1", "Status": "Active"}}
			if f.status != "" {
				list = append(list, map[string]string{"Id": pluginID, "Name": "Webhook", "Version": "22.0.0.0", "Status": f.status})
			}
			json.NewEncoder(w).Encode(list)
		case r.URL.Path == "/Plugins/"+pluginID+"/Configuration" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(f.conf)
		case r.URL.Path == "/Plugins/"+pluginID+"/Configuration" && r.Method == http.MethodPost:
			f.posts++
			var next map[string]any
			if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !f.drop {
				f.conf = next
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func genericOptions(t *testing.T, conf map[string]any) []map[string]any {
	t.Helper()
	raw, _ := json.Marshal(conf["GenericOptions"])
	var out []map[string]any
	json.Unmarshal(raw, &out)
	return out
}

func TestConfigureWebhookSetsUpEverythingCleanupNeeds(t *testing.T) {
	f := &fakeWebhookPlugin{status: "Active", conf: map[string]any{
		"ServerUrl": "http://jf:8096", // a plugin setting this code knows nothing about
		"GenericOptions": []any{
			// Another user's webhook: must survive.
			map[string]any{"WebhookName": "Home Assistant", "WebhookUri": "http://ha:8123/api/webhook/x"},
			// The broken destination found on the live box: no name, no template, no header.
			map[string]any{"WebhookUri": "http://127.0.0.1:1990/webhook/jellyfin", "NotificationTypes": []any{}},
		},
	}}
	c := New(f.serve(t).URL, "key")

	version, err := c.ConfigureWebhook(context.Background(), "http://127.0.0.1:1990/webhook/jellyfin", "X-JellyFreedom-Token", "s3cret")
	if err != nil {
		t.Fatalf("ConfigureWebhook: %v", err)
	}
	if version != "22.0.0.0" {
		t.Errorf("version = %q", version)
	}
	if f.conf["ServerUrl"] != "http://jf:8096" {
		t.Errorf("an unrelated plugin setting was lost: %v", f.conf["ServerUrl"])
	}

	dests := genericOptions(t, f.conf)
	if len(dests) != 2 {
		t.Fatalf("got %d destinations, want the other user's plus ours: %v", len(dests), dests)
	}
	if dests[0]["WebhookName"] != "Home Assistant" {
		t.Errorf("another destination was changed or dropped: %v", dests[0])
	}
	ours := dests[1]
	tmpl, _ := base64.StdEncoding.DecodeString(ours["Template"].(string))
	var payload WebhookPayload
	if err := json.Unmarshal([]byte(strings.NewReplacer("{{NotificationType}}", "PlaybackStop",
		"{{ItemId}}", "abc", "{{ItemType}}", "Movie").Replace(string(tmpl))), &payload); err != nil {
		t.Errorf("the template does not render to JSON the orchestrator can parse: %v (%s)", err, tmpl)
	} else if payload.NotificationType != "PlaybackStop" || payload.ItemID != "abc" || payload.ItemType != "Movie" {
		t.Errorf("the template lost a field: %+v", payload)
	}
	for k, want := range map[string]any{"EnableMovies": true, "EnableEpisodes": true, "EnableWebhook": true,
		"WebhookUri": "http://127.0.0.1:1990/webhook/jellyfin"} {
		if ours[k] != want {
			t.Errorf("%s = %v, want %v", k, ours[k], want)
		}
	}
	if types, _ := ours["NotificationTypes"].([]any); len(types) != 1 || types[0] != "PlaybackStop" {
		t.Errorf("NotificationTypes = %v, want only PlaybackStop", ours["NotificationTypes"])
	}
	if h, _ := ours["Headers"].([]any); len(h) != 1 ||
		h[0].(map[string]any)["Key"] != "X-JellyFreedom-Token" || h[0].(map[string]any)["Value"] != "s3cret" {
		t.Errorf("Headers = %v, want the secret in X-JellyFreedom-Token", ours["Headers"])
	}

	// A second run replaces ours instead of adding another.
	if _, err := c.ConfigureWebhook(context.Background(), "http://127.0.0.1:1990/webhook/jellyfin", "X-JellyFreedom-Token", "s3cret"); err != nil {
		t.Fatal(err)
	}
	if n := len(genericOptions(t, f.conf)); n != 2 {
		t.Errorf("a re-run left %d destinations, want 2", n)
	}
}

func TestConfigureWebhookNeedsTheActivePlugin(t *testing.T) {
	for _, status := range []string{"", "Restart", "Disabled"} {
		f := &fakeWebhookPlugin{status: status, conf: map[string]any{}}
		_, err := New(f.serve(t).URL, "key").ConfigureWebhook(context.Background(), "http://h/webhook/jellyfin", "X", "s")
		if !errors.Is(err, ErrNoWebhookPlugin) {
			t.Errorf("status %q: err = %v, want ErrNoWebhookPlugin", status, err)
		}
		if f.posts != 0 {
			t.Errorf("status %q: wrote a configuration anyway", status)
		}
	}
}

// Jellyfin answers 204 to a configuration it cannot deserialise and keeps the old one.
// Without reading it back, that reads as success.
func TestConfigureWebhookNoticesWhenJellyfinDropsIt(t *testing.T) {
	f := &fakeWebhookPlugin{status: "Active", conf: map[string]any{"GenericOptions": []any{}}, drop: true}
	_, err := New(f.serve(t).URL, "key").ConfigureWebhook(context.Background(), "http://h/webhook/jellyfin", "X", "s")
	if err == nil || !strings.Contains(err.Error(), "did not keep") {
		t.Errorf("err = %v, want a did-not-keep error", err)
	}
}

func TestConfigureWebhookKeepsTheKeyOutOfErrors(t *testing.T) {
	f := &fakeWebhookPlugin{status: "Active", conf: map[string]any{}}
	_, err := New(f.serve(t).URL, "wrong-key-value").ConfigureWebhook(context.Background(), "http://h/webhook/jellyfin", "X", "s")
	if err == nil {
		t.Fatal("a rejected key reported success")
	}
	if strings.Contains(err.Error(), "wrong-key-value") {
		t.Errorf("the API key leaked into the error: %v", err)
	}
}
