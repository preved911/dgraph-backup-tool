package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/preved911/resourcelock/ydb"
	ydbenv "github.com/ydb-platform/ydb-go-sdk-auth-environ"
	ydbsdk "github.com/ydb-platform/ydb-go-sdk/v3"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/klog"

	"github.com/sputnik-systems/dgraph-export-tool/internal/dgraph/export"
)

func main() {
	klog.InitFlags(nil)

	dgraphEndpointURL := flag.String("dgraph.endpoint-url", "http://localhost:8080/admin", "Dgraph instance admin endpoint")
	dgraphExportDest := flag.String("dgraph.export-dest", "", "Dgraph export export destination url")
	dgraphExportPeriod := flag.Duration("dgraph.export-period", time.Hour, "Dgraph export period")
	dgraphExportTmpPrefix := flag.String("dgraph.export-tmp-prefix", "/tmp", "Dgraph export temporary dir prefix")
	dgraphExportTmpPattern := flag.String("dgraph.export-tmp-pattern", `export[0-9]+`, "Dgraph export temporary files name pattern")
	dgraphExportTmpCleanup := flag.Bool("dgraph.export-tmp-cleanup", false, "Dgraph export temporary dir cleanup")
	ydbDatabaseName := flag.String("ydb.database-name", "", "YDB database name for init connection")
	ydbTableName := flag.String("ydb.table-name", "", "YDB table name")
	ydbLeaseName := flag.String("ydb.lease-name", "", "YDB lease name")
	leaseDuration := flag.Duration("leaderelection.lease-duration", 15*time.Second, "LeaderElection lease duration")
	renewDeadline := flag.Duration("leaderelection.renew-deadline", 10*time.Second, "LeaderElection renew deadline")
	retryPeriod := flag.Duration("leaderelection.retry-period", 2*time.Second, "LeaderElection retry period")

	flag.Parse()

	params := dgraphParams{
		endpoint:  *dgraphEndpointURL,
		dest:      *dgraphExportDest,
		accessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		secretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		period:    *dgraphExportPeriod,
		dgraphTmp: dgraphTmp{
			prefix:  *dgraphExportTmpPrefix,
			pattern: *dgraphExportTmpPattern,
			cleanup: *dgraphExportTmpCleanup,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := ydbsdk.Open(ctx, "grpcs://ydb.serverless.yandexcloud.net:2135",
		ydbenv.WithEnvironCredentials(ctx),
		ydbsdk.WithDatabase(*ydbDatabaseName),
	)
	if err != nil {
		klog.Fatal(err)
	}
	defer db.Close(ctx)

	identity, err := os.Hostname()
	if err != nil {
		klog.Fatal(err)
	}

	lock := ydb.New(db, *ydbTableName, *ydbLeaseName, identity)
	lec := leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: *leaseDuration,
		RenewDeadline: *renewDeadline,
		RetryPeriod:   *retryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				params.exportLoop(ctx)
			},
			OnStoppedLeading: func() {
				klog.V(3).Infof("stopped leading")
			},
			OnNewLeader: func(identity string) {
				klog.Infof("%s is leader now", identity)
			},
		},
		Name: "Dgraph Export Tool",
	}
	le, err := leaderelection.NewLeaderElector(lec)
	if err != nil {
		klog.Fatal(err)
	}

	if err := lock.CreateTable(ctx); err != nil {
		klog.Fatal(err)
	}

	go params.apiHandler(ctx, cancel)

	le.Run(ctx)
}

type dgraphParams struct {
	endpoint  string
	dest      string
	accessKey string
	secretKey string
	period    time.Duration
	dgraphTmp
}

type dgraphTmp struct {
	prefix  string
	pattern string
	cleanup bool
}

func (p *dgraphParams) exportLoop(ctx context.Context) {
	klog.V(3).Info("started export loop")

	c, err := export.NewClient(p.endpoint, p.dest,
		export.WithAccessKey(p.accessKey),
		export.WithSecretKey(p.secretKey),
	)
	if err != nil {
		klog.Fatal(err)
	}

	for ticker := time.NewTicker(p.period); ; {
		select {
		case <-ticker.C:
			klog.Info("make export export request")

			resp, err := c.Export(ctx)
			if err != nil {
				klog.Error(err)
				continue
			}

			klog.Infof("exported files: %v", resp.GetFiles())

			if p.dgraphTmp.cleanup {
				if err := cleanupTmpFiles(ctx, p.dgraphTmp.prefix, p.dgraphTmp.pattern); err != nil {
					klog.Error(err)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (p *dgraphParams) apiHandler(ctx context.Context, cancel context.CancelFunc) {
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {})
	http.HandleFunc("/api/v1/export", p.apiExportHandler(ctx))
	if err := http.ListenAndServe(":8081", nil); err != nil {
		klog.Error(err)
	}

	klog.Info("http handler finished")

	cancel()
}

func (p *dgraphParams) apiExportHandler(ctx context.Context) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			c, err := export.NewClient(p.endpoint, p.dest,
				export.WithAccessKey(p.accessKey),
				export.WithSecretKey(p.secretKey),
			)
			if err != nil {
				fmt.Fprintln(w, err.Error())
				return
			}
			resp, err := c.Export(ctx)
			if err != nil {
				fmt.Fprintln(w, err.Error())
				return
			}

			b, err := json.Marshal(resp)
			if err != nil {
				fmt.Fprintln(w, err.Error())
				return
			}

			fmt.Fprintln(w, string(b))
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}

		if p.dgraphTmp.cleanup {
			if err := cleanupTmpFiles(ctx, p.dgraphTmp.prefix, p.dgraphTmp.pattern); err != nil {
				klog.Error(err)
			}
		}
	}
}

func cleanupTmpFiles(ctx context.Context, prefix, pattern string) error {
	entries, err := os.ReadDir(prefix)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		match, err := filepath.Match(pattern, entry.Name())
		if err != nil {
			return err
		}
		if entry.IsDir() && match {
			path := filepath.Join(prefix, entry.Name())
			klog.Infof("removing directory: %s", path)
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		}
	}

	return nil
}
