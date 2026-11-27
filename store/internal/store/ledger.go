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

// Entry is one write the store has accepted: a payment, an order or a
// Stardust award, filed under the caller's Idempotency-Key.
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

// Kinds of write, in the order an order makes them.
var kinds = []string{"payment", "order", "stardust"}

// Ledger keeps the store's writes. The store process itself holds no state.
type Ledger interface {
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
}

func NewMemoryLedger() *MemoryLedger { return &MemoryLedger{entries: map[string]Entry{}} }

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
