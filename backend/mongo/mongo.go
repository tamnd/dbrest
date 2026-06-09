// Package mongo is the MongoDB backend data plane. The database-free half
// (filter lowering, pipeline building, capabilities) lives in query.go,
// pipeline.go, and capabilities.go; this file and its siblings add the live
// driver (mongo.Client, introspection, execute, result).
//
// DSN format (standard MongoDB connection string):
//
//	mongodb://user:pass@host:27017/database
//	mongodb://host:27017/database
package mongo

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	driver "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/rpc"
)

const connectTimeout = 10 * time.Second

// Backend is the MongoDB implementation of the dbrest backend SPI.
type Backend struct {
	client   *driver.Client
	db       *driver.Database
	dbName   string
	version  Version
	topology Topology
	caps     backend.Capabilities
	funcs    rpc.Registry
}

// Open connects to MongoDB, reads the server version and topology, and returns
// a ready Backend. The URI must include the database name as the path component.
func Open(uri string) (*Backend, error) {
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	opts := options.Client().ApplyURI(uri)
	client, err := driver.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("connect mongodb: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, err
	}

	dbName := dbNameFromURI(uri)
	if dbName == "" {
		dbName = "test"
	}

	ver, topo := serverInfo(ctx, client)
	return &Backend{
		client:   client,
		db:       client.Database(dbName),
		dbName:   dbName,
		version:  ver,
		topology: topo,
		caps:     Capabilities(ver, topo),
	}, nil
}

// serverInfo reads the MongoDB server version and topology.
func serverInfo(ctx context.Context, client *driver.Client) (Version, Topology) {
	var result struct {
		Version string `bson:"version"`
	}
	_ = client.Database("admin").RunCommand(ctx, map[string]any{"buildInfo": 1}).Decode(&result)
	ver := ParseVersion(result.Version)

	var helloResult struct {
		SetName string `bson:"setName"`
		Msg     string `bson:"msg"`
	}
	_ = client.Database("admin").RunCommand(ctx, map[string]any{"hello": 1}).Decode(&helloResult)
	topo := TopologyStandalone
	if helloResult.SetName != "" {
		topo = TopologyReplicaSet
	} else if helloResult.Msg == "isdbgrid" {
		topo = TopologySharded
	}
	return ver, topo
}

// dbNameFromURI extracts the database name from a MongoDB URI path component.
func dbNameFromURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

// DB returns the underlying mongo.Database, for tests.
func (b *Backend) DB() *driver.Database { return b.db }

// Register installs a portable function registry.
func (b *Backend) Register(reg rpc.Registry) { b.funcs = reg }

// Functions returns the function registry, or an empty one.
func (b *Backend) Functions() rpc.Registry {
	if b.funcs == nil {
		return rpc.EmptyRegistry{}
	}
	return b.funcs
}

// Capabilities returns the computed capability tiers.
func (b *Backend) Capabilities() backend.Capabilities { return b.caps }

// Close disconnects the client.
func (b *Backend) Close() error {
	return b.client.Disconnect(context.Background())
}

// MapError converts a MongoDB driver error to a PostgREST-compatible API error.
func (b *Backend) MapError(err error) *pgerr.APIError {
	if err == nil {
		return nil
	}
	if driver.IsDuplicateKeyError(err) {
		return pgerr.ErrUniqueViolation(err.Error())
	}
	if driver.IsTimeout(err) {
		return pgerr.ErrInternal("mongodb: timeout: " + err.Error())
	}
	return pgerr.ErrInternal(err.Error())
}
