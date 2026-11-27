package store

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"slices"
)

// skin.json is the café chain the demo pretends to be: its cafés, menu,
// rewards and one member's order history. Swap it to re-skin the demo for
// another brand; nothing else names the brand.
//
//go:embed skin.json
var skinJSON []byte

// Skin is the store's fixed data. Nothing in it changes while the demo runs.
type Skin struct {
	Brand    Brand             `json:"brand"`
	NonVegan []string          `json:"nonVegan"`
	Cafes    []Cafe            `json:"cafes"`
	Menu     []MenuItem        `json:"menu"`
	Options  map[string]Option `json:"options"`
	Members  []Member          `json:"members"`
}

type Brand struct {
	Name          string `json:"name"`
	Rewards       string `json:"rewards"`
	Currency      string `json:"currency"`
	EarnPerCredit int    `json:"earnPerCredit"`
	OrderPrefix   string `json:"orderPrefix"`
}

type Cafe struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	World         string   `json:"world"`
	PickupMinutes int      `json:"pickupMinutes"`
	OutOfStock    []string `json:"outOfStock"`
}

type MenuItem struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	Price       float64  `json:"price"`
	Ingredients []string `json:"ingredients"`
	Options     []string `json:"options"`
}

func (m MenuItem) cents() int64 { return int64(math.Round(m.Price * 100)) }

// Option changes an item's ingredients: replace one, remove one, or add one.
type Option struct {
	Replace string `json:"replace,omitempty"`
	With    string `json:"with,omitempty"`
	Remove  string `json:"remove,omitempty"`
	Add     string `json:"add,omitempty"`
}

type Member struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Stardust int         `json:"stardust"`
	Rewards  []Reward    `json:"rewards"`
	Orders   []PastOrder `json:"orders"`
}

type Reward struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Rule         string  `json:"rule"`
	PercentOff   int     `json:"percentOff,omitempty"`
	MinItems     int     `json:"minItems,omitempty"`
	MinSubtotal  float64 `json:"minSubtotal,omitempty"`
	FreeDrink    bool    `json:"freeDrink,omitempty"`
	CreditsOff   float64 `json:"creditsOff,omitempty"`
	StardustCost int     `json:"stardustCost,omitempty"`
}

type PastOrder struct {
	Number   string `json:"number"`
	PlacedAt string `json:"placedAt"`
	CafeID   string `json:"cafeId"`
	Lines    []Line `json:"lines"`
}

// Line is one line of a cart or an order: an item, how many, its options, and
// an optional note such as who it is for.
type Line struct {
	ItemID  string   `json:"itemId"`
	Qty     int      `json:"qty"`
	Options []string `json:"options"`
	Note    string   `json:"note,omitempty"`
}

// LoadSkin parses the embedded skin and checks that it refers only to things it defines.
func LoadSkin() (*Skin, error) {
	var s Skin
	if err := json.Unmarshal(skinJSON, &s); err != nil {
		return nil, fmt.Errorf("skin.json: %w", err)
	}
	for _, item := range s.Menu {
		for _, o := range item.Options {
			if _, ok := s.Options[o]; !ok {
				return nil, fmt.Errorf("skin.json: %s offers unknown option %q", item.ID, o)
			}
		}
	}
	for _, m := range s.Members {
		for _, o := range m.Orders {
			if s.cafe(o.CafeID) == nil {
				return nil, fmt.Errorf("skin.json: order %s names unknown cafe %q", o.Number, o.CafeID)
			}
			for _, l := range o.Lines {
				if s.item(l.ItemID) == nil {
					return nil, fmt.Errorf("skin.json: order %s names unknown item %q", o.Number, l.ItemID)
				}
			}
		}
	}
	return &s, nil
}

func (s *Skin) cafe(id string) *Cafe {
	i := slices.IndexFunc(s.Cafes, func(c Cafe) bool { return c.ID == id })
	if i < 0 {
		return nil
	}
	return &s.Cafes[i]
}

func (s *Skin) item(id string) *MenuItem {
	i := slices.IndexFunc(s.Menu, func(m MenuItem) bool { return m.ID == id })
	if i < 0 {
		return nil
	}
	return &s.Menu[i]
}

func (s *Skin) member(id string) *Member {
	i := slices.IndexFunc(s.Members, func(m Member) bool { return m.ID == id })
	if i < 0 {
		return nil
	}
	return &s.Members[i]
}

// ingredients returns what a line's item is made of once its options apply.
func (s *Skin) ingredients(item *MenuItem, options []string) []string {
	out := slices.Clone(item.Ingredients)
	for _, name := range options {
		o := s.Options[name]
		switch {
		case o.Replace != "":
			for i, ing := range out {
				if ing == o.Replace {
					out[i] = o.With
				}
			}
		case o.Remove != "":
			out = slices.DeleteFunc(out, func(ing string) bool { return ing == o.Remove })
		case o.Add != "":
			out = append(out, o.Add)
		}
	}
	return out
}

func (s *Skin) vegan(ingredients []string) bool {
	return !slices.ContainsFunc(ingredients, func(ing string) bool { return slices.Contains(s.NonVegan, ing) })
}
