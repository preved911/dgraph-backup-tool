package xsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ydb-platform/ydb-go-sdk/v3/internal/scheme/helpers"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/xcontext"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/xerrors"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/xsql/badconn"
	"github.com/ydb-platform/ydb-go-sdk/v3/retry"
	"github.com/ydb-platform/ydb-go-sdk/v3/scheme"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/result"
	"github.com/ydb-platform/ydb-go-sdk/v3/trace"
)

type connOption func(*conn)

func withFakeTxModes(modes ...QueryMode) connOption {
	return func(c *conn) {
		for _, m := range modes {
			c.beginTxFuncs[m] = c.beginTxFake
		}
	}
}

func withDataOpts(dataOpts ...options.ExecuteDataQueryOption) connOption {
	return func(c *conn) {
		c.dataOpts = dataOpts
	}
}

func withScanOpts(scanOpts ...options.ExecuteScanQueryOption) connOption {
	return func(c *conn) {
		c.scanOpts = scanOpts
	}
}

func withDefaultTxControl(defaultTxControl *table.TransactionControl) connOption {
	return func(c *conn) {
		c.defaultTxControl = defaultTxControl
	}
}

func withDefaultQueryMode(mode QueryMode) connOption {
	return func(c *conn) {
		c.defaultQueryMode = mode
	}
}

func withTrace(t *trace.DatabaseSQL) connOption {
	return func(c *conn) {
		c.trace = t
	}
}

type beginTxFunc func(ctx context.Context, txOptions driver.TxOptions) (currentTx, error)

type conn struct {
	connector *Connector
	trace     *trace.DatabaseSQL
	session   table.ClosableSession // Immutable and r/o usage.

	beginTxFuncs map[QueryMode]beginTxFunc

	closed           uint32
	lastUsage        int64
	defaultQueryMode QueryMode

	defaultTxControl *table.TransactionControl
	dataOpts         []options.ExecuteDataQueryOption

	scanOpts []options.ExecuteScanQueryOption

	currentTx currentTx
}

func (c *conn) GetDatabaseName() string {
	return c.connector.parent.Name()
}

func (c *conn) CheckNamedValue(*driver.NamedValue) error {
	// on this stage allows all values
	return nil
}

func (c *conn) IsValid() bool {
	return c.isReady()
}

type currentTx interface {
	driver.Tx
	driver.ExecerContext
	driver.QueryerContext
	table.TransactionIdentifier
}

type resultNoRows struct{}

func (resultNoRows) LastInsertId() (int64, error) { return 0, ErrUnsupported }
func (resultNoRows) RowsAffected() (int64, error) { return 0, ErrUnsupported }

var (
	_ driver.Conn               = &conn{}
	_ driver.ConnPrepareContext = &conn{}
	_ driver.ConnBeginTx        = &conn{}
	_ driver.ExecerContext      = &conn{}
	_ driver.QueryerContext     = &conn{}
	_ driver.Pinger             = &conn{}
	_ driver.Validator          = &conn{}
	_ driver.NamedValueChecker  = &conn{}

	_ driver.Result = resultNoRows{}
)

func newConn(c *Connector, s table.ClosableSession, opts ...connOption) *conn {
	cc := &conn{
		connector: c,
		session:   s,
	}
	cc.beginTxFuncs = map[QueryMode]beginTxFunc{
		DataQueryMode: cc.beginTx,
	}
	for _, o := range opts {
		if o != nil {
			o(cc)
		}
	}
	c.attach(cc)
	return cc
}

func (c *conn) isReady() bool {
	return c.session.Status() == table.SessionReady
}

func (c *conn) PrepareContext(ctx context.Context, query string) (_ driver.Stmt, err error) {
	onDone := trace.DatabaseSQLOnConnPrepare(c.trace, &ctx, query)
	defer func() {
		onDone(err)
	}()
	if !c.isReady() {
		return nil, badconn.Map(xerrors.WithStackTrace(errNotReadyConn))
	}
	return &stmt{
		conn:  c,
		query: query,
		trace: c.trace,
	}, nil
}

func (c *conn) sinceLastUsage() time.Duration {
	return time.Since(time.Unix(atomic.LoadInt64(&c.lastUsage), 0))
}

func (c *conn) execContext(ctx context.Context, query string, args []driver.NamedValue) (_ driver.Result, err error) {
	var (
		m      = queryModeFromContext(ctx, c.defaultQueryMode)
		onDone = trace.DatabaseSQLOnConnExec(c.trace, &ctx, query, m.String(), xcontext.IsIdempotent(ctx), c.sinceLastUsage())
	)

	defer func() {
		atomic.StoreInt64(&c.lastUsage, time.Now().Unix())
		onDone(err)
	}()
	switch m {
	case DataQueryMode:
		var (
			res    result.Result
			params *table.QueryParameters
		)
		query, params, err = c.normalize(query, args...)
		if err != nil {
			return nil, xerrors.WithStackTrace(err)
		}
		_, res, err = c.session.Execute(ctx,
			txControl(ctx, c.defaultTxControl),
			query, params, c.dataQueryOptions(ctx)...,
		)
		if err != nil {
			return nil, badconn.Map(xerrors.WithStackTrace(err))
		}
		defer func() {
			_ = res.Close()
		}()
		if err = res.NextResultSetErr(ctx); !xerrors.Is(err, nil, io.EOF) {
			return nil, badconn.Map(xerrors.WithStackTrace(err))
		}
		if err = res.Err(); err != nil {
			return nil, badconn.Map(xerrors.WithStackTrace(err))
		}
		return resultNoRows{}, nil
	case SchemeQueryMode:
		query, _, err = c.normalize(query)
		if err != nil {
			return nil, xerrors.WithStackTrace(err)
		}
		err = c.session.ExecuteSchemeQuery(ctx, query)
		if err != nil {
			return nil, badconn.Map(xerrors.WithStackTrace(err))
		}
		return resultNoRows{}, nil
	case ScriptingQueryMode:
		var (
			res    result.StreamResult
			params *table.QueryParameters
		)
		query, params, err = c.normalize(query, args...)
		if err != nil {
			return nil, xerrors.WithStackTrace(err)
		}
		res, err = c.connector.parent.Scripting().StreamExecute(ctx, query, params)
		if err != nil {
			return nil, badconn.Map(xerrors.WithStackTrace(err))
		}
		defer func() {
			_ = res.Close()
		}()
		if err = res.NextResultSetErr(ctx); !xerrors.Is(err, nil, io.EOF) {
			return nil, badconn.Map(xerrors.WithStackTrace(err))
		}
		if err = res.Err(); err != nil {
			return nil, badconn.Map(xerrors.WithStackTrace(err))
		}
		return resultNoRows{}, nil
	default:
		return nil, fmt.Errorf("unsupported query mode '%s' for execute query", m)
	}
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (_ driver.Result, err error) {
	if !c.isReady() {
		return nil, badconn.Map(xerrors.WithStackTrace(errNotReadyConn))
	}
	if c.currentTx != nil {
		return c.currentTx.ExecContext(ctx, query, args)
	}
	return c.execContext(ctx, query, args)
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (_ driver.Rows, err error) {
	if !c.isReady() {
		return nil, badconn.Map(xerrors.WithStackTrace(errNotReadyConn))
	}
	if c.currentTx != nil {
		return c.currentTx.QueryContext(ctx, query, args)
	}
	return c.queryContext(ctx, query, args)
}

func (c *conn) queryContext(ctx context.Context, query string, args []driver.NamedValue) (_ driver.Rows, err error) {
	m := queryModeFromContext(ctx, c.defaultQueryMode)
	onDone := trace.DatabaseSQLOnConnQuery(
		c.trace,
		&ctx,
		query,
		m.String(),
		xcontext.IsIdempotent(ctx),
		c.sinceLastUsage(),
	)
	defer func() {
		atomic.StoreInt64(&c.lastUsage, time.Now().Unix())
		onDone(err)
	}()
	switch m {
	case DataQueryMode:
		var (
			res    result.Result
			params *table.QueryParameters
		)
		query, params, err = c.normalize(query, args...)
		if err != nil {
			return nil, xerrors.WithStackTrace(err)
		}
		_, res, err = c.session.Execute(ctx,
			txControl(ctx, c.defaultTxControl),
			query, params, c.dataQueryOptions(ctx)...,
		)
		if err != nil {
			return nil, badconn.Map(xerrors.WithStackTrace(err))
		}
		if err = res.Err(); err != nil {
			return nil, badconn.Map(xerrors.WithStackTrace(err))
		}
		return &rows{
			conn:   c,
			result: res,
		}, nil
	case ScanQueryMode:
		var (
			res    result.StreamResult
			params *table.QueryParameters
		)
		query, params, err = c.normalize(query, args...)
		if err != nil {
			return nil, xerrors.WithStackTrace(err)
		}
		res, err = c.session.StreamExecuteScanQuery(ctx,
			query, params, c.scanQueryOptions(ctx)...,
		)
		if err != nil {
			return nil, badconn.Map(xerrors.WithStackTrace(err))
		}
		if err = res.Err(); err != nil {
			return nil, badconn.Map(xerrors.WithStackTrace(err))
		}
		return &rows{
			conn:   c,
			result: res,
		}, nil
	case ExplainQueryMode:
		var exp table.DataQueryExplanation
		query, _, err = c.normalize(query, args...)
		if err != nil {
			return nil, xerrors.WithStackTrace(err)
		}
		exp, err = c.session.Explain(ctx, query)
		if err != nil {
			return nil, badconn.Map(xerrors.WithStackTrace(err))
		}
		return &single{
			values: []sql.NamedArg{
				sql.Named("AST", exp.AST),
				sql.Named("Plan", exp.Plan),
			},
		}, nil
	case ScriptingQueryMode:
		var (
			res    result.StreamResult
			params *table.QueryParameters
		)
		query, params, err = c.normalize(query, args...)
		if err != nil {
			return nil, xerrors.WithStackTrace(err)
		}
		res, err = c.connector.parent.Scripting().StreamExecute(ctx, query, params)
		if err != nil {
			return nil, badconn.Map(xerrors.WithStackTrace(err))
		}
		if err = res.Err(); err != nil {
			return nil, badconn.Map(xerrors.WithStackTrace(err))
		}
		return &rows{
			conn:   c,
			result: res,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported query mode '%s' on conn query", m)
	}
}

func (c *conn) Ping(ctx context.Context) (err error) {
	onDone := trace.DatabaseSQLOnConnPing(c.trace, &ctx)
	defer func() {
		onDone(err)
	}()
	if !c.isReady() {
		return badconn.Map(xerrors.WithStackTrace(errNotReadyConn))
	}
	if err = c.session.KeepAlive(ctx); err != nil {
		return badconn.Map(xerrors.WithStackTrace(err))
	}
	return nil
}

func (c *conn) Close() (err error) {
	if atomic.CompareAndSwapUint32(&c.closed, 0, 1) {
		c.connector.detach(c)
		onDone := trace.DatabaseSQLOnConnClose(c.trace)
		defer func() {
			onDone(err)
		}()
		err = c.session.Close(context.Background())
		if err != nil {
			return badconn.Map(xerrors.WithStackTrace(err))
		}
		return nil
	}
	return badconn.Map(xerrors.WithStackTrace(errConnClosedEarly))
}

func (c *conn) Prepare(string) (driver.Stmt, error) {
	return nil, errDeprecated
}

func (c *conn) Begin() (driver.Tx, error) {
	return nil, errDeprecated
}

func (c *conn) normalize(q string, args ...driver.NamedValue) (query string, _ *table.QueryParameters, _ error) {
	return c.connector.Bindings.RewriteQuery(q, func() (ii []interface{}) {
		for i := range args {
			ii = append(ii, args[i])
		}
		return ii
	}()...)
}

func (c *conn) BeginTx(ctx context.Context, txOptions driver.TxOptions) (tx driver.Tx, err error) {
	var transaction table.Transaction
	onDone := trace.DatabaseSQLOnConnBegin(c.trace, &ctx)
	defer func() {
		onDone(transaction, err)
	}()

	if c.currentTx != nil {
		return nil, xerrors.WithStackTrace(
			fmt.Errorf("conn already have an opened currentTx: %s", c.currentTx.ID()),
		)
	}

	m := queryModeFromContext(ctx, c.defaultQueryMode)

	beginTx, isKnown := c.beginTxFuncs[m]
	if !isKnown {
		return nil, badconn.Map(xerrors.WithStackTrace(fmt.Errorf("wrong query mode: %s", m.String())))
	}

	tx, err = beginTx(ctx, txOptions)
	if err != nil {
		return nil, xerrors.WithStackTrace(err)
	}

	return tx, nil
}

func (c *conn) Version(context.Context) (_ string, err error) {
	const version = "default"
	return version, nil
}

func (c *conn) IsTableExists(ctx context.Context, tableName string) (tableExists bool, err error) {
	tableName = c.normalizePath(tableName)
	tableExists, err = helpers.IsEntryExists(ctx,
		c.connector.parent.Scheme(), tableName,
		scheme.EntryTable, scheme.EntryColumnTable,
	)
	if err != nil {
		return false, xerrors.WithStackTrace(err)
	}
	return tableExists, nil
}

func (c *conn) IsColumnExists(ctx context.Context, tableName, columnName string) (columnExists bool, err error) {
	tableName = c.normalizePath(tableName)
	tableExists, err := helpers.IsEntryExists(ctx,
		c.connector.parent.Scheme(), tableName,
		scheme.EntryTable, scheme.EntryColumnTable,
	)
	if err != nil {
		return false, xerrors.WithStackTrace(err)
	}
	if !tableExists {
		return false, xerrors.WithStackTrace(fmt.Errorf("table '%s' not exist", tableName))
	}

	err = retry.Retry(ctx, func(ctx context.Context) (err error) {
		desc, err := c.session.DescribeTable(ctx, tableName)
		if err != nil {
			return err
		}
		for i := range desc.Columns {
			if desc.Columns[i].Name == columnName {
				columnExists = true
				break
			}
		}
		return nil
	}, retry.WithIdempotent(true))
	if err != nil {
		return false, xerrors.WithStackTrace(err)
	}
	return columnExists, nil
}

func (c *conn) GetColumns(ctx context.Context, tableName string) (columns []string, err error) {
	tableName = c.normalizePath(tableName)
	tableExists, err := helpers.IsEntryExists(ctx,
		c.connector.parent.Scheme(), tableName,
		scheme.EntryTable, scheme.EntryColumnTable,
	)
	if err != nil {
		return nil, xerrors.WithStackTrace(err)
	}
	if !tableExists {
		return nil, xerrors.WithStackTrace(fmt.Errorf("table '%s' not exist", tableName))
	}

	err = retry.Retry(ctx, func(ctx context.Context) (err error) {
		desc, err := c.session.DescribeTable(ctx, tableName)
		if err != nil {
			return err
		}
		for i := range desc.Columns {
			columns = append(columns, desc.Columns[i].Name)
		}
		return nil
	}, retry.WithIdempotent(true))
	if err != nil {
		return nil, xerrors.WithStackTrace(err)
	}
	return columns, nil
}

func (c *conn) GetColumnType(ctx context.Context, tableName, columnName string) (dataType string, err error) {
	tableName = c.normalizePath(tableName)
	tableExists, err := helpers.IsEntryExists(ctx,
		c.connector.parent.Scheme(), tableName,
		scheme.EntryTable, scheme.EntryColumnTable,
	)
	if err != nil {
		return "", xerrors.WithStackTrace(err)
	}
	if !tableExists {
		return "", xerrors.WithStackTrace(fmt.Errorf("table '%s' not exist", tableName))
	}

	columnExist, err := c.IsColumnExists(ctx, tableName, columnName)
	if err != nil {
		return "", xerrors.WithStackTrace(err)
	}
	if !columnExist {
		return "", xerrors.WithStackTrace(fmt.Errorf("column '%s' not exist in table '%s'", columnName, tableName))
	}

	err = retry.Retry(ctx, func(ctx context.Context) (err error) {
		desc, err := c.session.DescribeTable(ctx, tableName)
		if err != nil {
			return err
		}
		for i := range desc.Columns {
			if desc.Columns[i].Name == columnName {
				dataType = desc.Columns[i].Type.Yql()
				break
			}
		}
		return nil
	}, retry.WithIdempotent(true))
	if err != nil {
		return "", xerrors.WithStackTrace(err)
	}
	return dataType, nil
}

func (c *conn) GetPrimaryKeys(ctx context.Context, tableName string) (pkCols []string, err error) {
	tableName = c.normalizePath(tableName)
	tableExists, err := helpers.IsEntryExists(ctx,
		c.connector.parent.Scheme(), tableName,
		scheme.EntryTable, scheme.EntryColumnTable,
	)
	if err != nil {
		return nil, xerrors.WithStackTrace(err)
	}
	if !tableExists {
		return nil, xerrors.WithStackTrace(fmt.Errorf("table '%s' not exist", tableName))
	}

	err = retry.Retry(ctx, func(ctx context.Context) (err error) {
		desc, err := c.session.DescribeTable(ctx, tableName)
		if err != nil {
			return err
		}
		pkCols = append(pkCols, desc.PrimaryKey...)
		return nil
	}, retry.WithIdempotent(true))
	if err != nil {
		return nil, xerrors.WithStackTrace(err)
	}
	return pkCols, nil
}

func (c *conn) IsPrimaryKey(ctx context.Context, tableName, columnName string) (ok bool, err error) {
	tableName = c.normalizePath(tableName)
	tableExists, err := helpers.IsEntryExists(ctx,
		c.connector.parent.Scheme(), tableName,
		scheme.EntryTable, scheme.EntryColumnTable,
	)
	if err != nil {
		return false, xerrors.WithStackTrace(err)
	}
	if !tableExists {
		return false, xerrors.WithStackTrace(fmt.Errorf("table '%s' not exist", tableName))
	}

	columnExist, err := c.IsColumnExists(ctx, tableName, columnName)
	if err != nil {
		return false, xerrors.WithStackTrace(err)
	}
	if !columnExist {
		return false, xerrors.WithStackTrace(fmt.Errorf("column '%s' not exist in table '%s'", columnName, tableName))
	}

	pkCols, err := c.GetPrimaryKeys(ctx, tableName)
	if err != nil {
		return false, xerrors.WithStackTrace(err)
	}
	for _, pkCol := range pkCols {
		if pkCol == columnName {
			ok = true
			break
		}
	}
	return ok, nil
}

func (c *conn) normalizePath(folderOrTable string) (absPath string) {
	return c.connector.PathNormalizer.NormalizePath(folderOrTable)
}

func (c *conn) GetTables(ctx context.Context, folder string) (tables []string, err error) {
	absPath := c.normalizePath(folder)
	var e scheme.Entry
	err = retry.Retry(ctx, func(ctx context.Context) (err error) {
		e, err = c.connector.parent.Scheme().DescribePath(ctx, absPath)
		return err
	}, retry.WithIdempotent(true))

	if err != nil {
		return nil, xerrors.WithStackTrace(err)
	}

	switch {
	case e.IsTable():
		_, table := path.Split(absPath)
		tables = append(tables, table)
		return tables, nil
	case e.IsDirectory():
		var d scheme.Directory
		err = retry.Retry(ctx, func(ctx context.Context) (err error) {
			d, err = c.connector.parent.Scheme().ListDirectory(ctx, absPath)
			return err
		}, retry.WithIdempotent(true))

		if err != nil {
			return nil, xerrors.WithStackTrace(err)
		}

		for i := range d.Children {
			if d.Children[i].IsTable() {
				tables = append(tables, d.Children[i].Name)
			}
		}
		return tables, nil
	default:
		return nil, xerrors.WithStackTrace(fmt.Errorf("path '%s' should be a table or directory", folder))
	}
}

func (c *conn) GetAllTables(ctx context.Context, folder string) (tables []string, err error) {
	folder = c.normalizePath(folder)
	ignoreDirs := map[string]bool{
		".sys":        true,
		".sys_health": true,
	}

	canEnter := func(dir string) bool {
		if _, ignore := ignoreDirs[dir]; ignore {
			return false
		}
		return true
	}

	queue := make([]string, 0)
	queue = append(queue, folder)

	for st := 0; st < len(queue); st++ {
		curPath := queue[st]

		var e scheme.Entry
		err = retry.Retry(ctx, func(ctx context.Context) (err error) {
			e, err = c.connector.parent.Scheme().DescribePath(ctx, curPath)
			return err
		}, retry.WithIdempotent(true))

		if err != nil {
			return nil, xerrors.WithStackTrace(err)
		}

		if e.IsTable() {
			switch curPath {
			case folder:
				_, table := path.Split(curPath)
				tables = append(tables, table)
			default:
				tables = append(tables, strings.TrimPrefix(curPath, folder+"/"))
			}
			continue
		}

		var d scheme.Directory
		err = retry.Retry(ctx, func(ctx context.Context) (err error) {
			d, err = c.connector.parent.Scheme().ListDirectory(ctx, curPath)
			return err
		}, retry.WithIdempotent(true))

		if err != nil {
			return nil, xerrors.WithStackTrace(err)
		}

		for i := range d.Children {
			if d.Children[i].IsDirectory() || d.Children[i].IsTable() {
				if canEnter(d.Children[i].Name) {
					queue = append(queue, path.Join(curPath, d.Children[i].Name))
				}
			}
		}
	}
	return tables, nil
}

func (c *conn) GetIndexes(ctx context.Context, tableName string) (indexes []string, err error) {
	tableName = c.normalizePath(tableName)
	tableExists, err := helpers.IsEntryExists(ctx,
		c.connector.parent.Scheme(), tableName,
		scheme.EntryTable, scheme.EntryColumnTable,
	)
	if err != nil {
		return nil, xerrors.WithStackTrace(err)
	}
	if !tableExists {
		return nil, xerrors.WithStackTrace(fmt.Errorf("table '%s' not exist", tableName))
	}

	err = retry.Retry(ctx, func(ctx context.Context) (err error) {
		desc, err := c.session.DescribeTable(ctx, tableName)
		if err != nil {
			return err
		}
		for i := range desc.Indexes {
			indexes = append(indexes, desc.Indexes[i].Name)
		}
		return nil
	}, retry.WithIdempotent(true))
	if err != nil {
		return nil, xerrors.WithStackTrace(err)
	}

	return indexes, nil
}

func (c *conn) GetIndexColumns(ctx context.Context, tableName, indexName string) (columns []string, err error) {
	tableName = c.normalizePath(tableName)
	tableExists, err := helpers.IsEntryExists(ctx,
		c.connector.parent.Scheme(), tableName,
		scheme.EntryTable, scheme.EntryColumnTable,
	)
	if err != nil {
		return nil, xerrors.WithStackTrace(err)
	}
	if !tableExists {
		return nil, xerrors.WithStackTrace(fmt.Errorf("table '%s' not exist", tableName))
	}

	err = retry.Retry(ctx, func(ctx context.Context) (err error) {
		desc, err := c.session.DescribeTable(ctx, tableName)
		if err != nil {
			return err
		}
		for i := range desc.Indexes {
			if desc.Indexes[i].Name == indexName {
				columns = append(columns, desc.Indexes[i].IndexColumns...)
				return nil
			}
		}
		return xerrors.WithStackTrace(fmt.Errorf("index '%s' not found in table '%s'", indexName, tableName))
	}, retry.WithIdempotent(true))
	if err != nil {
		return nil, xerrors.WithStackTrace(err)
	}

	return columns, nil
}
