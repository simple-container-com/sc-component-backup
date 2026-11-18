// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package notify

import (
	"context"
	"sync"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pkg/errors"
)

// TelegramSettings is the operator-provided Telegram config. Token is
// a secret; it goes out-of-band on GET responses (the SPA only reads
// token_set, see AppSettings).
type TelegramSettings struct {
	Token  string `bson:"token"   json:"-"`
	ChatID string `bson:"chatId"  json:"chat_id"`
}

// Store is the SettingsStore contract. Two impls — in-memory for
// local dev and unit tests, Mongo for production. A single-row model:
// one document per settings kind (today: telegram). Future webhook /
// email settings add their own typed get/put pairs rather than widen
// this interface into a generic key/value bag — keeps the SPA's
// AppSettings mapping obvious at the handler layer.
type Store interface {
	GetTelegram(ctx context.Context) (TelegramSettings, bool, error)
	PutTelegram(ctx context.Context, s TelegramSettings) error
	DeleteTelegram(ctx context.Context) error
}

// InMemoryStore is the test + local-dev impl.
type InMemoryStore struct {
	mu       sync.RWMutex
	telegram *TelegramSettings
}

func NewInMemoryStore() *InMemoryStore { return &InMemoryStore{} }

func (s *InMemoryStore) GetTelegram(_ context.Context) (TelegramSettings, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.telegram == nil {
		return TelegramSettings{}, false, nil
	}
	return *s.telegram, true, nil
}

func (s *InMemoryStore) PutTelegram(_ context.Context, t TelegramSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := t
	s.telegram = &cp
	return nil
}

func (s *InMemoryStore) DeleteTelegram(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.telegram = nil
	return nil
}

// MongoStore persists settings in a dedicated collection keyed by a
// settings-kind string ("telegram"). Plaintext token in v1 — the
// Mongo cluster is operator-admin only; promoting the token to an SC
// SecretsManager-backed secret is a follow-up when multi-operator
// comes online.
type MongoStore struct {
	client     *mongo.Client
	collection *mongo.Collection
}

const (
	settingsDBName         = "sc_component_backup"
	settingsCollectionName = "settings"
	telegramDocID          = "telegram"
)

func NewMongoStore(ctx context.Context, uri string) (*MongoStore, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, errors.Wrap(err, "mongo connect")
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, errors.Wrap(err, "mongo ping")
	}
	coll := client.Database(settingsDBName).Collection(settingsCollectionName)
	return &MongoStore{client: client, collection: coll}, nil
}

func (s *MongoStore) Close(ctx context.Context) error {
	if s.client == nil {
		return nil
	}
	return s.client.Disconnect(ctx)
}

func (s *MongoStore) GetTelegram(ctx context.Context) (TelegramSettings, bool, error) {
	var row struct {
		ID       string           `bson:"_id"`
		Telegram TelegramSettings `bson:",inline"`
	}
	err := s.collection.FindOne(ctx, bson.M{"_id": telegramDocID}).Decode(&row)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return TelegramSettings{}, false, nil
		}
		return TelegramSettings{}, false, errors.Wrap(err, "mongo find telegram")
	}
	return row.Telegram, true, nil
}

func (s *MongoStore) PutTelegram(ctx context.Context, t TelegramSettings) error {
	_, err := s.collection.ReplaceOne(
		ctx,
		bson.M{"_id": telegramDocID},
		bson.M{"_id": telegramDocID, "token": t.Token, "chatId": t.ChatID},
		options.Replace().SetUpsert(true),
	)
	if err != nil {
		return errors.Wrap(err, "mongo put telegram")
	}
	return nil
}

func (s *MongoStore) DeleteTelegram(ctx context.Context) error {
	_, err := s.collection.DeleteOne(ctx, bson.M{"_id": telegramDocID})
	if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		return errors.Wrap(err, "mongo delete telegram")
	}
	return nil
}
