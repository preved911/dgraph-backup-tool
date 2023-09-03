# ydb-go-sdk-auth-environ

> helpers to connect to YDB using environ 

## Installation <a name="Installation"></a>

```bash
go get -u github.com/ydb-platform/ydb-go-sdk-auth-environ
```

## Usage <a name="Usage"></a>

```go
import (
	env "github.com/ydb-platform/ydb-go-sdk-auth-environ"
)
...
    db, err := ydb.New(
        ctx,
        connectParams,
        env.WithEnvironCredentials(ctx), 
    )
    
```

## Auth environment variables

Name | Type     | Default | yandex-cloud | Description
-----|----------|---------|-----|----
`YDB_ANONYMOUS_CREDENTIALS` | `0` or `1` | `0`     | `-` | flag for use anonymous credentials
`YDB_METADATA_CREDENTIALS` | `0` or `1` |         | `+` | flag for use metadata credentials
`YDB_SERVICE_ACCOUNT_KEY_FILE_CREDENTIALS` | `string` |         | `+` | path to service account key file credentials
`YDB_ACCESS_TOKEN_CREDENTIALS` | `string` |         | `+/-` | use access token for authenticate with YDB. For authenticate with YDB inside yandex-cloud use short-life IAM-token. Other YDB installations can use access token depending on authenticate method  
