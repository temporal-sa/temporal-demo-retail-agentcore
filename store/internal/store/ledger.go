package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Entry is one write the store has accepted: a payment, an order, a
// Stardust award, a refund or a comp, filed under the caller's Idempotency-Key.
type Entry struct {
	Kind        string          `json:"kind"`
	Key         string          `json:"key"`
	RequestHash string          `json:"-"`
	Result      json.RawMessage `json:"result"`
	// Replays counts requests that arrived again with the same key and got
	// this stored result instead of a second write.
	Replays   int       `json:"replays"`
	CreatedAt time.Time `json:"createdAt"`
}

// Kinds of write: an order makes the first three, a support chat the others.
var kinds = []string{"payment", "order", "stardust", "refund", "comp"}

// Remediation is one thing support did for an order: a refund, a comp or
// Stardust, filed by the support chat's Idempotency-Key.
type Remediation struct {
	Kind   string    `json:"kind"`
	Key    string    `json:"key"`
	Amount Money     `json:"amount"`
	Items  []string  `json:"items"`
	Points int       `json:"points,omitempty"`
	Reason string    `json:"reason"`
	Ref    string    `json:"ref"` // the refund ID or the comp's order number
	At     time.Time `json:"at"`
}

// Case is an order's support record: everything support has done for it,
// whichever chat did it.
type Case struct {
	Refunded     Money         `json:"refunded"`
	Remediations []Remediation `json:"remediations"`
}

// ErrOverRefund means a refund would take the order's refunds past what
// was paid.
var ErrOverRefund = errors.New("refunds would exceed the amount paid")

// Ledger keeps the store's writes. The store process itself holds no state.
type Ledger interface {
	// Remediate adds r to the order's case, once per r.Key. A refund is
	// added only while the case's refunds stay within refundCap; the check
	// and the write are one atomic step, so two chats can't both refund
	// the last of the payment.
	Remediate(ctx context.Context, orderKey string, r Remediation, refundCap Money) error
	// Case returns the order's support record, empty if support did nothing.
	Case(ctx context.Context, orderKey string) (Case, error)
	// Record stores e unless an entry of its kind and key exists. It returns
	// the stored entry and whether this call created it.
	Record(ctx context.Context, e Entry) (Entry, bool, error)
	// List returns every entry filed under key, in kinds order.
	List(ctx context.Context, key string) ([]Entry, error)
}

// entryTTL is how long DynamoDB keeps an entry. A demo order needs minutes.
const entryTTL = 7 * 24 * time.Hour

// DynamoLedger keeps entries in one DynamoDB table with a string key "pk".
type DynamoLedger struct {
	db    *dynamodb.Client
	table string
}

func NewDynamoLedger(db *dynamodb.Client, table string) *DynamoLedger {
	return &DynamoLedger{db: db, table: table}
}

func pk(kind, key string) string { return kind + "#" + key }

func (l *DynamoLedger) Record(ctx context.Context, e Entry) (Entry, bool, error) {
	_, err := l.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &l.table,
		Item: map[string]types.AttributeValue{
			"pk":          &types.AttributeValueMemberS{Value: pk(e.Kind, e.Key)},
			"requestHash": &types.AttributeValueMemberS{Value: e.RequestHash},
			"result":      &types.AttributeValueMemberS{Value: string(e.Result)},
			"replays":     &types.AttributeValueMemberN{Value: "0"},
			"createdAt":   &types.AttributeValueMemberS{Value: e.CreatedAt.UTC().Format(time.RFC3339Nano)},
			"expiresAt":   &types.AttributeValueMemberN{Value: strconv.FormatInt(e.CreatedAt.Add(entryTTL).Unix(), 10)},
		},
		// The write happens once per key, whoever asks and however often.
		ConditionExpression: aws.String("attribute_not_exists(pk)"),
	})
	if err == nil {
		return e, true, nil
	}
	var exists *types.ConditionalCheckFailedException
	if !errors.As(err, &exists) {
		return Entry{}, false, err
	}
	out, err := l.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &l.table,
		Key:                       map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: pk(e.Kind, e.Key)}},
		UpdateExpression:          aws.String("ADD replays :one"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":one": &types.AttributeValueMemberN{Value: "1"}},
		ReturnValues:              types.ReturnValueAllNew,
	})
	if err != nil {
		return Entry{}, false, err
	}
	stored, err := decodeEntry(out.Attributes)
	return stored, false, err
}

func (l *DynamoLedger) List(ctx context.Context, key string) ([]Entry, error) {
	keys := make([]map[string]types.AttributeValue, 0, len(kinds))
	for _, k := range kinds {
		keys = append(keys, map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: pk(k, key)}})
	}
	out, err := l.db.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{
		RequestItems: map[string]types.KeysAndAttributes{l.table: {Keys: keys, ConsistentRead: aws.Bool(true)}},
	})
	if err != nil {
		return nil, err
	}
	byKind := map[string]Entry{}
	for _, item := range out.Responses[l.table] {
		e, err := decodeEntry(item)
		if err != nil {
			return nil, err
		}
		byKind[e.Kind] = e
	}
	return inKindOrder(byKind), nil
}

func casePK(orderKey string) string { return "case#" + orderKey }

func (l *DynamoLedger) Remediate(ctx context.Context, orderKey string, r Remediation, refundCap Money) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	amount := Money(0)
	if r.Kind == "refund" {
		amount = r.Amount
	}
	_, err = l.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &l.table,
		Key:       map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: casePK(orderKey)}},
		UpdateExpression: aws.String("SET refunded = if_not_exists(refunded, :zero) + :amount, " +
			"remediations = list_append(if_not_exists(remediations, :none), :this), expiresAt = :expires ADD remediationKeys :key"),
		// Once per chat, and never past the payment.
		ConditionExpression: aws.String("(attribute_not_exists(remediationKeys) OR NOT contains(remediationKeys, :keyValue)) AND " +
			"(attribute_not_exists(refunded) OR refunded <= :room)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":zero":     &types.AttributeValueMemberN{Value: "0"},
			":amount":   &types.AttributeValueMemberN{Value: strconv.FormatInt(int64(amount), 10)},
			":room":     &types.AttributeValueMemberN{Value: strconv.FormatInt(int64(refundCap-amount), 10)},
			":none":     &types.AttributeValueMemberL{Value: []types.AttributeValue{}},
			":this":     &types.AttributeValueMemberL{Value: []types.AttributeValue{&types.AttributeValueMemberS{Value: string(raw)}}},
			":key":      &types.AttributeValueMemberSS{Value: []string{r.Key}},
			":keyValue": &types.AttributeValueMemberS{Value: r.Key},
			":expires":  &types.AttributeValueMemberN{Value: strconv.FormatInt(r.At.Add(entryTTL).Unix(), 10)},
		},
	})
	var failed *types.ConditionalCheckFailedException
	if !errors.As(err, &failed) {
		return err
	}
	// Either this chat's remediation is already filed (a retry), or the
	// refund doesn't fit.
	c, err := l.Case(ctx, orderKey)
	if err != nil {
		return err
	}
	for _, done := range c.Remediations {
		if done.Key == r.Key {
			return nil
		}
	}
	return ErrOverRefund
}

func (l *DynamoLedger) Case(ctx context.Context, orderKey string) (Case, error) {
	out, err := l.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &l.table,
		Key:            map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: casePK(orderKey)}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return Case{}, err
	}
	c := Case{Remediations: []Remediation{}}
	if v, ok := out.Item["refunded"].(*types.AttributeValueMemberN); ok {
		n, _ := strconv.ParseInt(v.Value, 10, 64)
		c.Refunded = Money(n)
	}
	if list, ok := out.Item["remediations"].(*types.AttributeValueMemberL); ok {
		for _, v := range list.Value {
			s, ok := v.(*types.AttributeValueMemberS)
			if !ok {
				continue
			}
			var r Remediation
			if err := json.Unmarshal([]byte(s.Value), &r); err != nil {
				return Case{}, err
			}
			c.Remediations = append(c.Remediations, r)
		}
	}
	return c, nil
}

func decodeEntry(item map[string]types.AttributeValue) (Entry, error) {
	str := func(name string) string {
		if v, ok := item[name].(*types.AttributeValueMemberS); ok {
			return v.Value
		}
		return ""
	}
	var e Entry
	kind, key, ok := strings.Cut(str("pk"), "#")
	if !ok {
		return e, fmt.Errorf("ledger item has no valid pk")
	}
	e.Kind, e.Key = kind, key
	e.RequestHash = str("requestHash")
	e.Result = json.RawMessage(str("result"))
	if v, ok := item["replays"].(*types.AttributeValueMemberN); ok {
		e.Replays, _ = strconv.Atoi(v.Value)
	}
	e.CreatedAt, _ = time.Parse(time.RFC3339Nano, str("createdAt"))
	return e, nil
}

func inKindOrder(byKind map[string]Entry) []Entry {
	out := []Entry{}
	for _, k := range kinds {
		if e, ok := byKind[k]; ok {
			out = append(out, e)
		}
	}
	return out
}

// CreateTable makes the ledger table if it is missing. Only the local stack
// uses it; on the platform the operator creates the table.
func (l *DynamoLedger) CreateTable(ctx context.Context) error {
	_, err := l.db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            &l.table,
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		BillingMode:          types.BillingModePayPerRequest,
	})
	var inUse *types.ResourceInUseException
	if errors.As(err, &inUse) {
		return nil
	}
	return err
}

// MemoryLedger is a Ledger for tests.
type MemoryLedger struct {
	mu      sync.Mutex
	entries map[string]Entry
	cases   map[string]Case
}

func NewMemoryLedger() *MemoryLedger {
	return &MemoryLedger{entries: map[string]Entry{}, cases: map[string]Case{}}
}

func (l *MemoryLedger) Remediate(_ context.Context, orderKey string, r Remediation, refundCap Money) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.cases[orderKey]
	for _, done := range c.Remediations {
		if done.Key == r.Key {
			return nil
		}
	}
	if r.Kind == "refund" {
		if c.Refunded+r.Amount > refundCap {
			return ErrOverRefund
		}
		c.Refunded += r.Amount
	}
	c.Remediations = append(append([]Remediation{}, c.Remediations...), r)
	l.cases[orderKey] = c
	return nil
}

func (l *MemoryLedger) Case(_ context.Context, orderKey string) (Case, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.cases[orderKey]
	if c.Remediations == nil {
		c.Remediations = []Remediation{}
	}
	return c, nil
}

func (l *MemoryLedger) Record(_ context.Context, e Entry) (Entry, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if stored, ok := l.entries[pk(e.Kind, e.Key)]; ok {
		stored.Replays++
		l.entries[pk(e.Kind, e.Key)] = stored
		return stored, false, nil
	}
	l.entries[pk(e.Kind, e.Key)] = e
	return e, true, nil
}

func (l *MemoryLedger) List(_ context.Context, key string) ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	byKind := map[string]Entry{}
	for _, k := range kinds {
		if e, ok := l.entries[pk(k, key)]; ok {
			byKind[k] = e
		}
	}
	return inKindOrder(byKind), nil
}
