package crew

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// StoreClient reads from the store's REST API: the member's profile for the
// phone's header, and an order's ledger rows for the backstage panel.
type StoreClient struct {
	base, token string
	http        *http.Client

	mu      sync.Mutex
	profile *Profile
}

func NewStoreClient(base, token string) *StoreClient {
	return &StoreClient{base: base, token: token, http: &http.Client{Timeout: 3 * time.Second}}
}

// Profile is what the phone shows before any order: the brand, the member
// and the pickup café.
type Profile struct {
	Brand    map[string]any `json:"brand"`
	Member   string         `json:"member"`
	Stardust int            `json:"stardust"`
	Cafe     string         `json:"cafe"`
}

// Ledger is the store's record of one order's writes.
type Ledger struct {
	Entries []LedgerEntry `json:"entries"`
	Replays int           `json:"replays"`
}

type LedgerEntry struct {
	Kind      string          `json:"kind"`
	Result    json.RawMessage `json:"result"`
	Replays   int             `json:"replays"`
	CreatedAt time.Time       `json:"createdAt"`
}

func (c *StoreClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/store/v1"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("store %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Profile reads the profile once and keeps it: the skin does not change
// while the store runs. A failed read is tried again on the next call.
func (c *StoreClient) Profile(ctx context.Context, memberID, cafeID string) (*Profile, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.profile != nil {
		return c.profile, nil
	}
	var cafes struct {
		Brand map[string]any `json:"brand"`
		Cafes []struct {
			ID, Name string
		} `json:"cafes"`
	}
	var rewards struct {
		MemberName string `json:"memberName"`
		Stardust   int    `json:"stardust"`
	}
	if err := c.get(ctx, "/cafes", &cafes); err != nil {
		return nil, err
	}
	if err := c.get(ctx, "/members/"+url.PathEscape(memberID)+"/rewards", &rewards); err != nil {
		return nil, err
	}
	p := &Profile{Brand: cafes.Brand, Member: rewards.MemberName, Stardust: rewards.Stardust}
	for _, cafe := range cafes.Cafes {
		if cafe.ID == cafeID {
			p.Cafe = cafe.Name
		}
	}
	c.profile = p
	return p, nil
}

// SupportRecord is what support has done for an order, from every chat:
// the store's record, passed through to the page.
type SupportRecord = json.RawMessage

// Record returns the order's support record. Before the order is paid the
// store has none and answers 404, an error here.
func (c *StoreClient) Record(ctx context.Context, orderKey string) (SupportRecord, error) {
	var rec json.RawMessage
	if err := c.get(ctx, "/orders/"+url.PathEscape(orderKey)+"/support", &rec); err != nil {
		return nil, err
	}
	return rec, nil
}

func (c *StoreClient) Ledger(ctx context.Context, key string) (*Ledger, error) {
	var l Ledger
	if err := c.get(ctx, "/ledger/"+url.PathEscape(key), &l); err != nil {
		return nil, err
	}
	return &l, nil
}
