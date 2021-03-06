// Copyright (c) 2016-2019 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package cmd

import (
	"github.com/uber/kraken/build-index/tagclient"
	"github.com/uber/kraken/build-index/tagserver"
	"github.com/uber/kraken/build-index/tagstore"
	"github.com/uber/kraken/build-index/tagtype"
	"github.com/uber/kraken/lib/backend"
	"github.com/uber/kraken/lib/healthcheck"
	"github.com/uber/kraken/lib/hostlist"
	"github.com/uber/kraken/lib/persistedretry"
	"github.com/uber/kraken/lib/persistedretry/tagreplication"
	"github.com/uber/kraken/lib/persistedretry/writeback"
	"github.com/uber/kraken/lib/store"
	"github.com/uber/kraken/lib/upstream"
	"github.com/uber/kraken/localdb"
	"github.com/uber/kraken/metrics"
	"github.com/uber/kraken/nginx"
	"github.com/uber/kraken/origin/blobclient"
	"github.com/uber/kraken/utils/configutil"
	"github.com/uber/kraken/utils/log"

	"github.com/spf13/cobra"
)

var (
	port          int
	configFile    string
	krakenCluster string
	secretsFile   string

	rootCmd = &cobra.Command{
		Short: "kraken-index handles all tag related requests and cross cluster replications",
		Run: func(rootCmd *cobra.Command, args []string) {
			run()
		},
	}
)

func init() {
	rootCmd.PersistentFlags().IntVarP(
		&port, "port", "", 0, "tag server port")
	rootCmd.PersistentFlags().StringVarP(
		&configFile, "config", "", "", "configuration file path")
	rootCmd.PersistentFlags().StringVarP(
		&krakenCluster, "cluster", "", "", "cluster name (e.g. prod01-zone1)")
	rootCmd.PersistentFlags().StringVarP(
		&secretsFile, "secrets", "", "", "path to a secrets YAML file to load into configuration")
}

func Execute() {
	rootCmd.Execute()
}

func run() {
	if port == 0 {
		panic("must specify non-zero port")
	}

	var config Config
	if err := configutil.Load(configFile, &config); err != nil {
		panic(err)
	}
	if secretsFile != "" {
		if err := configutil.Load(secretsFile, &config); err != nil {
			panic(err)
		}
	}
	log.ConfigureLogger(config.ZapLogging)

	stats, closer, err := metrics.New(config.Metrics, krakenCluster)
	if err != nil {
		log.Fatalf("Failed to init metrics: %s", err)
	}
	defer closer.Close()

	go metrics.EmitVersion(stats)

	ss, err := store.NewSimpleStore(config.Store, stats)
	if err != nil {
		log.Fatalf("Error creating simple store: %s", err)
	}

	backends, err := backend.NewManager(config.Backends, config.Auth)
	if err != nil {
		log.Fatalf("Error creating backend manager: %s", err)
	}

	tls, err := config.TLS.BuildClient()
	if err != nil {
		log.Fatalf("Error building client tls config: %s", err)
	}

	origins, err := config.Origin.Build(upstream.WithHealthCheck(healthcheck.Default(tls)))
	if err != nil {
		log.Fatalf("Error building origin host list: %s", err)
	}

	r := blobclient.NewClientResolver(blobclient.NewProvider(blobclient.WithTLS(tls)), origins)
	originClient := blobclient.NewClusterClient(r)

	localOriginDNS, err := config.Origin.StableAddr()
	if err != nil {
		log.Fatalf("Error getting stable origin addr: %s", err)
	}

	localDB, err := localdb.New(config.LocalDB)
	if err != nil {
		log.Fatalf("Error creating local db: %s", err)
	}

	cluster, err := config.Cluster.Build(upstream.WithHealthCheck(healthcheck.Default(tls)))
	if err != nil {
		log.Fatalf("Error building cluster host list: %s", err)
	}
	neighbors, err := hostlist.StripLocal(cluster, port)
	if err != nil {
		log.Fatalf("Error stripping local machine from cluster list: %s", err)
	}

	remotes, err := config.Remotes.Build()
	if err != nil {
		log.Fatalf("Error building remotes from configuration: %s", err)
	}

	tagReplicationExecutor := tagreplication.NewExecutor(
		stats,
		originClient,
		tagclient.NewProvider(tls))
	tagReplicationStore, err := tagreplication.NewStore(localDB, remotes)
	if err != nil {
		log.Fatalf("Error creating tag replication store: %s", err)
	}
	tagReplicationManager, err := persistedretry.NewManager(
		config.TagReplication,
		stats,
		tagReplicationStore,
		tagReplicationExecutor)
	if err != nil {
		log.Fatalf("Error creating tag replication manager: %s", err)
	}

	writeBackManager, err := persistedretry.NewManager(
		config.WriteBack,
		stats,
		writeback.NewStore(localDB),
		writeback.NewExecutor(stats, ss, backends))
	if err != nil {
		log.Fatalf("Error creating write-back manager: %s", err)
	}

	tagStore := tagstore.New(config.TagStore, stats, ss, backends, writeBackManager)

	depResolver, err := tagtype.NewMap(config.TagTypes, originClient)
	if err != nil {
		log.Fatalf("Error creating tag type manager: %s", err)
	}

	server := tagserver.New(
		config.TagServer,
		stats,
		backends,
		localOriginDNS,
		originClient,
		neighbors,
		tagStore,
		remotes,
		tagReplicationManager,
		tagclient.NewProvider(tls),
		depResolver)
	go func() {
		log.Fatal(server.ListenAndServe())
	}()

	log.Info("Starting nginx...")
	log.Fatal(nginx.Run(
		config.Nginx,
		map[string]interface{}{
			"port":   port,
			"server": nginx.GetServer(config.TagServer.Listener.Net, config.TagServer.Listener.Addr),
		},
		nginx.WithTLS(config.TLS)))
}
