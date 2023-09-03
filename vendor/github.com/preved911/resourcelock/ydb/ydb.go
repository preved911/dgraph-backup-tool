package ydb

import (
	"context"
	"encoding/json"
	"fmt"
	"path"

	"github.com/ydb-platform/ydb-go-sdk/v3"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/types"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog"
)

type YDBLeaderElection struct {
	db       *ydb.Driver
	table    string
	name     string
	identity string
}

func New(db *ydb.Driver, table, name, identity string) *YDBLeaderElection {
	return &YDBLeaderElection{db, table, name, identity}
}

func (l *YDBLeaderElection) CreateTable(ctx context.Context) error {
	return l.db.Table().Do(ctx, func(ctx context.Context, s table.Session) (err error) {
		tablePath := path.Join(l.db.Name(), l.table)
		opts := []options.CreateTableOption{
			options.WithColumn("name", types.TypeString),
			options.WithColumn("value", types.Optional(types.TypeJSON)),
			options.WithPrimaryKeyColumn("name"),
		}
		return s.CreateTable(ctx, tablePath, opts...)
	})
}

func (l *YDBLeaderElection) Get(ctx context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	klog.V(3).Infof("get leaderelection record %s/%s", l.table, l.name)

	b, err := l.get(ctx)
	if err != nil {
		return nil, nil, err
	}

	var ler *resourcelock.LeaderElectionRecord
	if err := json.Unmarshal(b, &ler); err != nil {
		return nil, nil, err
	}

	return ler, b, nil
}

func (l *YDBLeaderElection) Create(ctx context.Context, ler resourcelock.LeaderElectionRecord) error {
	klog.V(2).Infof("create leaderelection record %s/%s", l.table, l.name)

	return l.Update(ctx, ler)
}

func (l *YDBLeaderElection) Update(ctx context.Context, ler resourcelock.LeaderElectionRecord) error {
	klog.V(2).Infof("update leaderelection record %s/%s", l.table, l.name)

	return l.update(ctx, ler)
}

func (l *YDBLeaderElection) RecordEvent(event string) {
	klog.Infof("eeaderelection event %s/%s: %s", l.table, l.name, event)
}

func (l *YDBLeaderElection) Identity() string {
	return l.identity
}

func (l *YDBLeaderElection) Describe() string {
	return fmt.Sprintf("%s/%s", l.table, l.name)
}

func (l *YDBLeaderElection) update(ctx context.Context, ler resourcelock.LeaderElectionRecord) error {
	return l.db.Table().DoTx(ctx, func(ctx context.Context, tx table.TransactionActor) (err error) {
		queryValue := fmt.Sprintf(`PRAGMA TablePathPrefix("%s");`, l.db.Name())
		queryValue += "DECLARE $name AS String;"
		queryValue += "DECLARE $value AS Json;"
		queryValue += fmt.Sprintf("UPSERT INTO %s (name, value) VALUES ($name, $value);", l.table)
		lerValue, err := json.Marshal(ler)
		if err != nil {
			return err
		}
		res, err := tx.Execute(ctx, queryValue, table.NewQueryParameters(
			table.ValueParam("$name", types.StringValueFromString(l.name)),
			table.ValueParam("$value", types.JSONValueFromBytes(lerValue)),
		))
		if err != nil {
			return err
		}
		if err = res.Err(); err != nil {
			return err
		}
		return res.Close()
	}, table.WithIdempotent())
}

func (l *YDBLeaderElection) get(ctx context.Context) ([]byte, error) {
	var lerValue *string
	err := l.db.Table().DoTx(ctx, func(ctx context.Context, tx table.TransactionActor) (err error) {
		queryValue := fmt.Sprintf(`PRAGMA TablePathPrefix("%s");`, l.db.Name())
		queryValue += fmt.Sprintf(`SELECT value FROM %s WHERE name = "%s"`, l.table, l.name)
		res, err := tx.Execute(ctx, queryValue, nil)
		if err != nil {
			return err
		}
		if err = res.Err(); err != nil {
			return err
		}
		defer res.Close()

		for res.NextResultSet(ctx) {
			if !res.HasNextRow() {
				return errors.NewNotFound(schema.GroupResource{}, l.name)
			}

			for res.NextRow() {
				return res.Scan(&lerValue)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return []byte(*lerValue), nil
}
