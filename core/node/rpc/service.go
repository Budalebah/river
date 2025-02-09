package rpc

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/river-build/river/core/node/auth"
	"github.com/river-build/river/core/node/config"
	"github.com/river-build/river/core/node/crypto"
	"github.com/river-build/river/core/node/events"
	"github.com/river-build/river/core/node/infra"
	"github.com/river-build/river/core/node/nodes"
	. "github.com/river-build/river/core/node/protocol/protocolconnect"
	"github.com/river-build/river/core/node/registries"
	"github.com/river-build/river/core/node/storage"
)

var serviceRequests = infra.NewSuccessMetrics(infra.RPC_CATEGORY, nil)

type Service struct {
	// Context and config
	serverCtx     context.Context
	config        *config.Config
	instanceId    string
	defaultLogger *slog.Logger
	wallet        *crypto.Wallet
	startTime     time.Time
	mode          string

	// exitSignal is used to report critical errors from background task and RPC handlers
	// that should cause the service to stop. For example, if new instance for
	// the same database is started, the old one should stop.
	exitSignal chan error

	// Storage
	storagePoolInfo *storage.PgxPoolInfo
	storage         storage.StreamStorage

	// Streams
	cache       events.StreamCache
	syncHandler SyncHandler

	// River chain
	riverChain       *crypto.Blockchain
	registryContract *registries.RiverRegistryContract
	nodeRegistry     nodes.NodeRegistry
	streamRegistry   nodes.StreamRegistry
	onChainConfig    crypto.OnChainConfiguration

	// Base chain
	chainAuth auth.ChainAuth

	// Network
	listener   net.Listener
	httpServer *http.Server
	mux        httpMux

	// Status string
	status atomic.Pointer[string]

	// Archiver if in archive mode
	Archiver *Archiver
}

var (
	_ StreamServiceHandler = (*Service)(nil)
	_ NodeToNodeHandler    = (*Service)(nil)
)

func (s *Service) ExitSignal() chan error {
	return s.exitSignal
}

func (s *Service) SetStatus(status string) {
	s.status.Store(&status)
}

func (s *Service) GetStatus() string {
	status := s.status.Load()
	if status == nil {
		return "STARTING"
	}
	return *status
}

func (s *Service) Storage() storage.StreamStorage {
	return s.storage
}
