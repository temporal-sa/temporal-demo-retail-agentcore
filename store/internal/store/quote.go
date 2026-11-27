package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// QuoteRequest is a cart to price at one café for one member.
type QuoteRequest struct {
	CafeID   string `json:"cafeId"`
	MemberID string `json:"memberId"`
	Lines    []Line `json:"lines"`
	// RewardID is a reward to apply, "best" for the one that saves the most,
	// or empty for none.
	RewardID string `json:"rewardId"`
}

// Quote is a priced cart. Its ID carries every figure, signed, so the store
// needs no record of it: a payment presents the ID and the store checks the
// signature instead of looking the quote up.
type Quote struct {
	ID           string          `json:"quoteId"`
	CafeID       string          `json:"cafeId"`
	CafeName     string          `json:"cafeName"`
	MemberID     string          `json:"memberId"`
	Lines        []PricedLine    `json:"lines"`
	ItemCount    int             `json:"itemCount"`
	Subtotal     Money           `json:"subtotal"`
	Reward       *AppliedReward  `json:"reward,omitempty"`
	Alternatives []AppliedReward `json:"alternatives"`
	Total        Money           `json:"total"`
	Earns        int             `json:"earns"`
	ReadyAt      time.Time       `json:"readyAt"`
	ExpiresAt    time.Time       `json:"expiresAt"`
}

// PricedLine is a cart line with its price and what it is made of.
type PricedLine struct {
	Line
	Name        string   `json:"name"`
	UnitPrice   Money    `json:"unitPrice"`
	LinePrice   Money    `json:"linePrice"`
	Ingredients []string `json:"ingredients"`
	Vegan       bool     `json:"vegan"`
}

// AppliedReward is a reward and what it takes off this cart.
type AppliedReward struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Discount Money  `json:"discount"`
	// Eligible is false, with the reason, when the cart doesn't qualify.
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason,omitempty"`
}

// Money is an amount in hundredths of a credit, written as credits.
type Money int64

func (m Money) MarshalJSON() ([]byte, error) { return fmt.Appendf(nil, "%d.%02d", m/100, m%100), nil }

func (m *Money) UnmarshalJSON(b []byte) error {
	var whole, frac int64
	if _, err := fmt.Sscanf(string(b), "%d.%02d", &whole, &frac); err != nil {
		return fmt.Errorf("money %s: %w", b, err)
	}
	*m = Money(whole*100 + frac)
	return nil
}

func (m Money) String() string { return fmt.Sprintf("%d.%02d", m/100, m%100) }

// quoteTTL is how long a member has to pay before the prices must be checked again.
const quoteTTL = 2 * time.Hour

// ValidationError is a request the store refuses as it is. Every problem is listed.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string { return strings.Join(e.Problems, "; ") }

// PriceCart prices a cart against the menu, the café's stock and the member's rewards.
func (s *Skin) PriceCart(req QuoteRequest, now time.Time) (*Quote, error) {
	var problems []string
	cafe := s.cafe(req.CafeID)
	if cafe == nil {
		problems = append(problems, fmt.Sprintf("unknown cafe %q", req.CafeID))
	}
	member := s.member(req.MemberID)
	if member == nil {
		problems = append(problems, fmt.Sprintf("unknown member %q", req.MemberID))
	}
	if len(req.Lines) == 0 {
		problems = append(problems, "the cart has no lines")
	}
	if len(req.Lines) > 40 {
		problems = append(problems, "a cart has at most 40 lines")
	}
	if len(problems) > 0 {
		return nil, &ValidationError{problems}
	}

	q := &Quote{CafeID: cafe.ID, CafeName: cafe.Name, MemberID: member.ID, Alternatives: []AppliedReward{}}
	for i, l := range req.Lines {
		item := s.item(l.ItemID)
		if item == nil {
			problems = append(problems, fmt.Sprintf("line %d: unknown item %q", i+1, l.ItemID))
			continue
		}
		if l.Qty < 1 || l.Qty > 20 {
			problems = append(problems, fmt.Sprintf("line %d: qty must be 1 to 20", i+1))
		}
		for _, o := range l.Options {
			if !slices.Contains(item.Options, o) {
				problems = append(problems, fmt.Sprintf("line %d: %s does not offer option %q (it offers %s)", i+1, item.Name, o, optionList(item.Options)))
			}
		}
		ings := s.ingredients(item, l.Options)
		for _, ing := range ings {
			if slices.Contains(cafe.OutOfStock, ing) {
				problems = append(problems, fmt.Sprintf("line %d: %s is out of %s today", i+1, cafe.Name, ing))
			}
		}
		options := l.Options
		if options == nil {
			options = []string{}
		}
		unit := Money(item.cents())
		q.Lines = append(q.Lines, PricedLine{
			Line:        Line{ItemID: item.ID, Qty: l.Qty, Options: options, Note: l.Note},
			Name:        item.Name,
			UnitPrice:   unit,
			LinePrice:   unit * Money(l.Qty),
			Ingredients: ings,
			Vegan:       s.vegan(ings),
		})
		q.ItemCount += l.Qty
		q.Subtotal += unit * Money(l.Qty)
	}
	if len(problems) > 0 {
		return nil, &ValidationError{problems}
	}

	for _, r := range member.Rewards {
		q.Alternatives = append(q.Alternatives, s.applyReward(r, member, q))
	}
	switch req.RewardID {
	case "":
	case "best":
		for i, a := range q.Alternatives {
			if a.Eligible && (q.Reward == nil || a.Discount > q.Reward.Discount) {
				q.Reward = &q.Alternatives[i]
			}
		}
	default:
		i := slices.IndexFunc(q.Alternatives, func(a AppliedReward) bool { return a.ID == req.RewardID })
		switch {
		case i < 0:
			return nil, &ValidationError{[]string{fmt.Sprintf("member has no reward %q", req.RewardID)}}
		case !q.Alternatives[i].Eligible:
			return nil, &ValidationError{[]string{fmt.Sprintf("reward %q does not apply: %s", req.RewardID, q.Alternatives[i].Reason)}}
		}
		q.Reward = &q.Alternatives[i]
	}

	q.Total = q.Subtotal
	if q.Reward != nil {
		q.Total -= q.Reward.Discount
	}
	q.Earns = int(int64(q.Total) * int64(s.Brand.EarnPerCredit) / 100)
	q.ReadyAt = now.Add(time.Duration(cafe.PickupMinutes) * time.Minute).UTC().Truncate(time.Minute)
	q.ExpiresAt = now.Add(quoteTTL).UTC().Truncate(time.Second)
	return q, nil
}

func (s *Skin) applyReward(r Reward, m *Member, q *Quote) AppliedReward {
	a := AppliedReward{ID: r.ID, Name: r.Name}
	switch {
	case r.PercentOff > 0:
		switch {
		case q.ItemCount < r.MinItems:
			a.Reason = fmt.Sprintf("needs %d items, the cart has %d", r.MinItems, q.ItemCount)
		case q.Subtotal < Money(r.MinSubtotal*100):
			a.Reason = fmt.Sprintf("needs a subtotal of at least %.2f", r.MinSubtotal)
		default:
			a.Eligible = true
			// Rounded to the nearest hundredth, halves up.
			a.Discount = (q.Subtotal*Money(r.PercentOff) + 50) / 100
		}
	case r.FreeDrink:
		for _, l := range q.Lines {
			if s.item(l.ItemID).Category == "drink" && l.UnitPrice > a.Discount {
				a.Discount = l.UnitPrice
			}
		}
		a.Eligible = a.Discount > 0
		if !a.Eligible {
			a.Reason = "the cart has no drink"
		}
	case r.CreditsOff > 0:
		if m.Stardust < r.StardustCost {
			a.Reason = fmt.Sprintf("needs %d Stardust, the member has %d", r.StardustCost, m.Stardust)
		} else {
			a.Eligible = true
			a.Discount = min(Money(r.CreditsOff*100), q.Subtotal)
		}
	}
	return a
}

func optionList(opts []string) string {
	if len(opts) == 0 {
		return "no options"
	}
	return strings.Join(opts, ", ")
}

// Signer makes and checks quote IDs. The key never leaves the store.
type Signer struct{ key []byte }

func NewSigner(key []byte) Signer { return Signer{key: key} }

// Sign returns the quote with its ID set: the quote as JSON, then an HMAC of it.
func (sg Signer) Sign(q *Quote) error {
	q.ID = ""
	body, err := json.Marshal(q)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, sg.key)
	mac.Write(body)
	q.ID = "q1." + base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return nil
}

var (
	errBadQuote     = errors.New("the quote ID is not one this store issued")
	errExpiredQuote = errors.New("the quote has expired; price the cart again")
)

// Verify returns the quote an ID carries, if this store signed it and it has not expired.
func (sg Signer) Verify(id string, now time.Time) (*Quote, error) {
	parts := strings.Split(id, ".")
	if len(parts) != 3 || parts[0] != "q1" {
		return nil, errBadQuote
	}
	body, err1 := base64.RawURLEncoding.DecodeString(parts[1])
	sum, err2 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil {
		return nil, errBadQuote
	}
	mac := hmac.New(sha256.New, sg.key)
	mac.Write(body)
	if !hmac.Equal(sum, mac.Sum(nil)) {
		return nil, errBadQuote
	}
	var q Quote
	if err := json.Unmarshal(body, &q); err != nil {
		return nil, errBadQuote
	}
	if now.After(q.ExpiresAt) {
		return nil, errExpiredQuote
	}
	q.ID = id
	return &q, nil
}
