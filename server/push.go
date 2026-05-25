package server

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// PushSubscription mirrors the browser PushSubscription.toJSON() format.
type PushSubscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// PushStore manages VAPID keys and push subscriptions.
type PushStore struct {
	mu           sync.RWMutex
	publicKey    string
	privateKey   string
	subs         []PushSubscription
	vapidPath    string
	subsPath     string
}

func NewPushStore(vapidPath, subsPath string) *PushStore {
	ps := &PushStore{vapidPath: vapidPath, subsPath: subsPath}

	// Load or generate VAPID keys.
	if data, err := os.ReadFile(vapidPath); err == nil {
		var keys struct {
			Public  string `json:"publicKey"`
			Private string `json:"privateKey"`
		}
		if json.Unmarshal(data, &keys) == nil && keys.Public != "" {
			ps.publicKey = keys.Public
			ps.privateKey = keys.Private
		}
	}
	if ps.publicKey == "" {
		priv, pub, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			log.Fatalf("[push] generate VAPID keys: %v", err)
		}
		ps.publicKey = pub
		ps.privateKey = priv
		data, _ := json.MarshalIndent(map[string]string{
			"publicKey": pub, "privateKey": priv,
		}, "", "  ")
		_ = os.WriteFile(vapidPath, data, 0o644)
		log.Printf("[push] generated new VAPID keys")
	}

	// Load existing subscriptions.
	if data, err := os.ReadFile(subsPath); err == nil {
		var subs []PushSubscription
		if json.Unmarshal(data, &subs) == nil {
			ps.subs = subs
		}
	}

	log.Printf("[push] loaded %d subscription(s)", len(ps.subs))
	return ps
}

func (ps *PushStore) saveSubs() {
	data, _ := json.MarshalIndent(ps.subs, "", "  ")
	_ = os.WriteFile(ps.subsPath, data, 0o644)
}

func (ps *PushStore) PublicKey() string {
	return ps.publicKey
}

const maxSubs = 10 // keep at most 10 subscriptions (oldest dropped first)

func (ps *PushStore) Subscribe(sub PushSubscription) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	// Deduplicate by endpoint — update keys if subscription was refreshed.
	for i, s := range ps.subs {
		if s.Endpoint == sub.Endpoint {
			ps.subs[i] = sub
			ps.saveSubs()
			return
		}
	}
	ps.subs = append(ps.subs, sub)
	// Trim oldest entries beyond the cap.
	if len(ps.subs) > maxSubs {
		ps.subs = ps.subs[len(ps.subs)-maxSubs:]
	}
	ps.saveSubs()
	log.Printf("[push] new subscription, total: %d", len(ps.subs))
}

func (ps *PushStore) Unsubscribe(endpoint string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for i, s := range ps.subs {
		if s.Endpoint == endpoint {
			ps.subs = append(ps.subs[:i], ps.subs[i+1:]...)
			ps.saveSubs()
			return
		}
	}
}

// SendAll sends a push notification to all registered subscriptions.
// Removes expired subscriptions (410 Gone). Safe to call from goroutines.
func (ps *PushStore) SendAll(title, body string) {
	ps.mu.RLock()
	// Copy subscriptions so we can release the read lock.
	subs := make([]PushSubscription, len(ps.subs))
	copy(subs, ps.subs)
	ps.mu.RUnlock()

	log.Printf("[push] SendAll to %d subscription(s): %s", len(subs), title)
	if len(subs) == 0 {
		log.Printf("[push] no subscriptions, skipping")
		return
	}

	payload, _ := json.Marshal(map[string]string{"title": title, "body": body})

	var expired []string
	for _, sub := range subs {
		s := &webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys: webpush.Keys{
				P256dh: sub.Keys.P256dh,
				Auth:   sub.Keys.Auth,
			},
		}
		resp, err := webpush.SendNotification(payload, s, &webpush.Options{
			VAPIDPublicKey:  ps.publicKey,
			VAPIDPrivateKey: ps.privateKey,
			Subscriber:      "mailto:push@localhost",
			TTL:             60,
		})
		if err != nil {
			log.Printf("[push] send error for %s: %v", sub.Endpoint[:40], err)
			continue
		}
		resp.Body.Close()
		log.Printf("[push] sent to %s… status=%d", sub.Endpoint[:min(40, len(sub.Endpoint))], resp.StatusCode)
		if resp.StatusCode == 410 || resp.StatusCode == 404 {
			expired = append(expired, sub.Endpoint)
		}
	}

	// Remove expired subscriptions.
	if len(expired) > 0 {
		ps.mu.Lock()
		for _, ep := range expired {
			for i, s := range ps.subs {
				if s.Endpoint == ep {
					ps.subs = append(ps.subs[:i], ps.subs[i+1:]...)
					break
				}
			}
		}
		ps.saveSubs()
		ps.mu.Unlock()
		log.Printf("[push] removed %d expired subscription(s)", len(expired))
	}
}

// ── HTTP handlers ────────────────────────────────────────────

func (s *Server) handlePushVAPIDKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]string{"publicKey": s.push.PublicKey()})
}

func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var sub PushSubscription
		if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if sub.Endpoint == "" {
			http.Error(w, "endpoint required", http.StatusBadRequest)
			return
		}
		s.push.Subscribe(sub)
		writeJSON(w, map[string]any{"ok": true})

	case http.MethodDelete:
		var body struct {
			Endpoint string `json:"endpoint"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		s.push.Unsubscribe(body.Endpoint)
		writeJSON(w, map[string]any{"ok": true})

	default:
		http.Error(w, "POST or DELETE", http.StatusMethodNotAllowed)
	}
}
