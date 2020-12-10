package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/grafana/tempo/cmd/tempo/app"
	tempodb_backend "github.com/grafana/tempo/tempodb/backend"
	"github.com/grafana/tempo/tempodb/backend/local"
	"github.com/grafana/tempo/tempodb/encoding"
	"gopkg.in/yaml.v2"

	"github.com/alecthomas/kong"
	"github.com/grafana/tempo/tempodb/backend/gcs"
	"github.com/grafana/tempo/tempodb/backend/s3"
)

type globalOptions struct {
	ConfigFile string `type:"path" short:"c" help:"Path to tempo config file"`
}

type backendOptions struct {
	Backend string `help:"backend to connect to (s3/gcs/local), optional, overrides backend in config file" enum:",s3,gcs,local"`
	Bucket  string `help:"bucket to scan, optional, overrides bucket in config file"`

	S3Endpoint string `name:"s3-endpoint" help:"s3 endpoint (s3.dualstack.us-east-2.amazonaws.com), optional, overrides endpoint in config file"`
	S3User     string `name:"s3-user" help:"s3 username, optional, overrides username in config file"`
	S3Pass     string `name:"s3-pass" help:"s3 password, optional, overrides password in config file"`
}

var cli struct {
	globalOptions

	List struct {
		Block  listBlockCmd  `cmd:"" help:"List information about a block"`
		Blocks listBlocksCmd `cmd:"" help:"List information about all blocks in a bucket"`
	} `cmd:""`

	Query queryCmd `cmd:"" help:"query tempo api"`
}

func main() {
	ctx := kong.Parse(&cli,
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{
			//Compact: true,
		}),
	)
	err := ctx.Run(&cli.globalOptions)
	ctx.FatalIfErrorf(err)
}

func loadBackend(b *backendOptions, g *globalOptions) (tempodb_backend.Reader, tempodb_backend.Compactor, error) {
	// Defaults
	cfg := app.Config{}
	cfg.RegisterFlagsAndApplyDefaults("", &flag.FlagSet{})

	// Existing config
	if g.ConfigFile != "" {
		buff, err := ioutil.ReadFile(g.ConfigFile)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read configFile %s: %w", g.ConfigFile, err)
		}

		err = yaml.UnmarshalStrict(buff, &cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse configFile %s: %w", g.ConfigFile, err)
		}
	}

	// cli overrides
	if b.Backend != "" {
		cfg.StorageConfig.Trace.Backend = b.Backend
	}

	if b.Bucket != "" {
		cfg.StorageConfig.Trace.Local.Path = b.Bucket
		cfg.StorageConfig.Trace.GCS.BucketName = b.Bucket
		cfg.StorageConfig.Trace.S3.Bucket = b.Bucket
	}

	if b.S3Endpoint != "" {
		cfg.StorageConfig.Trace.S3.Endpoint = b.S3Endpoint
	}

	var err error
	var r tempodb_backend.Reader
	var c tempodb_backend.Compactor

	switch cfg.StorageConfig.Trace.Backend {
	case "local":
		r, _, c, err = local.New(cfg.StorageConfig.Trace.Local)
	case "gcs":
		r, _, c, err = gcs.New(cfg.StorageConfig.Trace.GCS)
	case "s3":
		r, _, c, err = s3.New(cfg.StorageConfig.Trace.S3)
	default:
		err = fmt.Errorf("unknown backend %s", cfg.StorageConfig.Trace.Backend)
	}

	if err != nil {
		return nil, nil, err
	}

	return r, c, nil
}

type unifiedBlockMeta struct {
	id              uuid.UUID
	compactionLevel uint8
	objects         int
	window          int64
	start           time.Time
	end             time.Time
	compacted       bool
}

func getMeta(meta *encoding.BlockMeta, compactedMeta *encoding.CompactedBlockMeta, windowRange time.Duration) unifiedBlockMeta {
	if meta != nil {
		return unifiedBlockMeta{
			id:              meta.BlockID,
			compactionLevel: meta.CompactionLevel,
			objects:         meta.TotalObjects,
			window:          meta.EndTime.Unix() / int64(windowRange/time.Second),
			start:           meta.StartTime,
			end:             meta.EndTime,
			compacted:       false,
		}
	}
	if compactedMeta != nil {
		return unifiedBlockMeta{
			id:              compactedMeta.BlockID,
			compactionLevel: compactedMeta.CompactionLevel,
			objects:         compactedMeta.TotalObjects,
			window:          compactedMeta.EndTime.Unix() / int64(windowRange/time.Second),
			start:           compactedMeta.StartTime,
			end:             compactedMeta.EndTime,
			compacted:       true,
		}
	}
	return unifiedBlockMeta{
		id:              uuid.UUID{},
		compactionLevel: 0,
		objects:         -1,
		window:          -1,
		start:           time.Unix(0, 0),
		end:             time.Unix(0, 0),
		compacted:       false,
	}
}

// boundedWaitGroup like a normal wait group except limits number of active goroutines to given capacity.
type boundedWaitGroup struct {
	wg sync.WaitGroup
	ch chan struct{} // Chan buffer size is used to limit concurrency.
}

func newBoundedWaitGroup(cap int) boundedWaitGroup {
	return boundedWaitGroup{ch: make(chan struct{}, cap)}
}

func (bwg *boundedWaitGroup) Add(delta int) {
	for i := 0; i > delta; i-- {
		<-bwg.ch
	}
	for i := 0; i < delta; i++ {
		bwg.ch <- struct{}{}
	}
	bwg.wg.Add(delta)
}

func (bwg *boundedWaitGroup) Done() {
	bwg.Add(-1)
}

func (bwg *boundedWaitGroup) Wait() {
	bwg.wg.Wait()
}
