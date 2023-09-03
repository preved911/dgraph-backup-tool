# Usage
You can use it like this:
```
package main

import (
	"context"
	"flag"
	"os"
	"time"

	"github.com/preved911/resourcelock/ydb"
	ydbsdk "github.com/ydb-platform/ydb-go-sdk/v3"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/klog"
)

func main() {
	klog.InitFlags(nil)
	database := flag.String("db", "", "ydb database name for init connection")
	leaseDuration := flag.Duration("lease-duration", 15*time.Second, "lease duration")
	renewDeadline := flag.Duration("renew-deadline", 10*time.Second, "renew deadline")
	retryPeriod := flag.Duration("retry-period", 2*time.Second, "retry period")
	flag.Parse()

	token := os.Getenv("YC_TOKEN")
	ctx := context.Background()
	db, err := ydbsdk.Open(ctx, "grpcs://ydb.serverless.yandexcloud.net:2135",
		ydbsdk.WithAccessTokenCredentials(token),
		ydbsdk.WithDatabase(*database),
	)
	if err != nil {
		klog.Fatal(err)
	}
	defer db.Close(ctx)

	identity, err := os.Hostname()
	if err != nil {
		klog.Fatal(err)
	}
	lock := ydb.New(db, "testtable", "testrecord", identity)
	lec := leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: *leaseDuration,
		RenewDeadline: *renewDeadline,
		RetryPeriod:   *retryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				klog.Infof("%s started leading", lock.Identity())
			},
			OnStoppedLeading: func() {
				klog.Infof("%s stopped leading", lock.Identity())
			},
			OnNewLeader: func(identity string) {
				klog.Infof("leader changed")
			},
		},
		Name: "TestApp",
	}
	le, err := leaderelection.NewLeaderElector(lec)
	if err != nil {
		panic(err)
	}

	if err := lock.CreateTable(ctx); err != nil {
		klog.Fatal(err)
	}

	le.Run(ctx)
}
```
